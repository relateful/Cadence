package peercache

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/greed/cadence/internal/audiocache"
	"github.com/greed/cadence/internal/metrics"
)

const (
	gossipInterval = 30 * time.Second
	gossipTimeout  = 5 * time.Second
	pullTimeout    = 30 * time.Second
	maxPulls       = 3
	maxIndexKeys   = 200_000
)

type Pool struct {
	peers  []string
	token  string
	store  *audiocache.Store
	client *http.Client
	log    *slog.Logger

	mu    sync.RWMutex
	index map[string]string

	sem      chan struct{}
	inflight sync.Map
}

func New(peers []string, token string, store *audiocache.Store, log *slog.Logger) *Pool {
	clean := make([]string, 0, len(peers))
	for _, p := range peers {
		if p = strings.TrimRight(strings.TrimSpace(p), "/"); p != "" {
			clean = append(clean, p)
		}
	}
	return &Pool{
		peers:  clean,
		token:  token,
		store:  store,
		client: &http.Client{Timeout: pullTimeout},
		log:    log.With("component", "peercache"),
		index:  map[string]string{},
		sem:    make(chan struct{}, maxPulls),
	}
}

func (p *Pool) Enabled() bool { return p != nil && len(p.peers) > 0 && p.store != nil }

func (p *Pool) Run(ctx context.Context) {
	if !p.Enabled() {
		return
	}
	p.refresh(ctx)
	t := time.NewTicker(gossipInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.refresh(ctx)
		}
	}
}

func (p *Pool) refresh(ctx context.Context) {
	next := make(map[string]string)
	for _, peer := range p.peers {
		keys, err := p.fetchIndex(ctx, peer)
		if err != nil {
			metrics.PeerCache.Inc("gossip_failed")
			p.log.Debug("peer index refresh failed", "peer", peer, "error", err)
			continue
		}
		for _, k := range keys {
			if len(next) >= maxIndexKeys {
				break
			}
			if _, seen := next[k]; !seen {
				next[k] = peer
			}
		}
	}
	p.mu.Lock()
	p.index = next
	p.mu.Unlock()
}

func (p *Pool) fetchIndex(ctx context.Context, peer string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, gossipTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, peer+"/v1/cache/index", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", p.token)
	res, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("peer index status %d", res.StatusCode)
	}
	var keys []string
	if err := json.NewDecoder(io.LimitReader(res.Body, 16<<20)).Decode(&keys); err != nil {
		return nil, err
	}
	return keys, nil
}

func (p *Pool) MaybePull(keys []string) {
	if !p.Enabled() {
		return
	}
	for _, key := range keys {
		if p.store.Has(key) {
			return
		}
	}
	p.mu.RLock()
	var key, peer string
	for _, k := range keys {
		if src, ok := p.index[k]; ok {
			key, peer = k, src
			break
		}
	}
	p.mu.RUnlock()
	if key == "" {
		return
	}
	metrics.PeerCache.Inc("index_hit")
	if _, busy := p.inflight.LoadOrStore(key, struct{}{}); busy {
		return
	}
	select {
	case p.sem <- struct{}{}:
	default:
		p.inflight.Delete(key)
		metrics.PeerCache.Inc("too_busy")
		return
	}
	go func() {
		defer func() {
			<-p.sem
			p.inflight.Delete(key)
			if v := recover(); v != nil {
				metrics.PeerCache.Inc("failed")
				p.log.Error("peer cache pull panicked", "key", key, "peer", peer, "panic", v)
			}
		}()
		p.pull(key, peer)
	}()
}

func (p *Pool) pull(key, peer string) {
	ctx, cancel := context.WithTimeout(context.Background(), pullTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, peer+"/v1/cache/file?key="+key, nil)
	if err != nil {
		metrics.PeerCache.Inc("failed")
		return
	}
	req.Header.Set("Authorization", p.token)
	res, err := p.client.Do(req)
	if err != nil {
		metrics.PeerCache.Inc("failed")
		p.log.Debug("peer cache pull failed", "key", key, "peer", peer, "error", err)
		return
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK || res.ContentLength <= 0 {
		metrics.PeerCache.Inc("failed")
		return
	}
	w, err := p.store.Create(key, res.ContentLength)
	if err != nil {
		metrics.PeerCache.Inc("failed")
		return
	}
	if _, err := io.Copy(w, res.Body); err != nil {
		w.Abort()
		metrics.PeerCache.Inc("failed")
		p.log.Debug("peer cache copy failed", "key", key, "error", err)
		return
	}
	if err := w.Commit(); err != nil {
		metrics.PeerCache.Inc("failed")
		p.log.Debug("peer cache commit failed", "key", key, "error", err)
		return
	}
	metrics.PeerCache.Inc("pulled")
	p.log.Debug("pulled a track from a peer cache", "key", key, "peer", peer, "bytes", res.ContentLength)
}
