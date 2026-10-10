package source

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/greed/cadence/internal/cache"
	"github.com/greed/cadence/internal/metrics"
)

const (
	mirrorCacheSize = 200_000
	mirrorCacheTTL  = 14 * 24 * time.Hour
	missCacheTTL    = 30 * time.Minute
	loadCacheSize   = 20_000
	loadCacheTTL    = 10 * time.Minute
	prefetchPending = 64
	prefetchWorkers = 4
	prefetchTimeout = 30 * time.Second
)

var SearchAliases = []string{"ytmsearch:", "ytsearch:"}

type mirrorEntry struct {
	track Track
	ok    bool
}

type flight struct {
	done  chan struct{}
	track Track
	err   error
}

type Resolver struct {
	sources     map[string]Source
	ordered     []Source
	searchOrder []string
	audio       Playable
	mirrors     *cache.LRU[string, mirrorEntry]
	loads       *cache.LRU[string, LoadResult]
	mu          sync.Mutex
	inflight    map[string]*flight
	warming     map[string]struct{}
	warmSlots   chan struct{}
	log         *slog.Logger
}

func NewResolver(audio Playable, searchOrder []string, log *slog.Logger, sources ...Source) *Resolver {
	r := &Resolver{
		sources:     map[string]Source{},
		searchOrder: searchOrder,
		audio:       audio,
		mirrors:     cache.New[string, mirrorEntry](mirrorCacheSize, mirrorCacheTTL),
		loads:       cache.New[string, LoadResult](loadCacheSize, loadCacheTTL),
		inflight:    map[string]*flight{},
		warming:     map[string]struct{}{},
		warmSlots:   make(chan struct{}, prefetchWorkers),
		log:         log.With("component", "resolver"),
	}
	for _, s := range append([]Source{audio}, sources...) {
		if s == nil {
			continue
		}
		r.sources[s.Name()] = s
		r.ordered = append(r.ordered, s)
	}
	return r
}

func (r *Resolver) Sources() []string {
	out := make([]string, 0, len(r.ordered))
	for _, s := range r.ordered {
		out = append(out, s.Name())
	}
	return out
}

func (r *Resolver) byPrefix(identifier string) (Source, string, bool) {
	for _, s := range r.ordered {
		if p := s.SearchPrefix(); p != "" && strings.HasPrefix(identifier, p) {
			return s, strings.TrimSpace(strings.TrimPrefix(identifier, p)), true
		}
	}
	return nil, "", false
}

func isrcIdentifier(identifier string) (string, bool) {
	prefix, rest, found := strings.Cut(identifier, ":")
	if !found || !strings.HasSuffix(prefix, "isrc") {
		return "", false
	}
	isrc := NormalizeISRC(rest)
	return isrc, len(isrc) == 12
}

func NormalizeISRC(raw string) string {
	return strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(raw), "-", ""))
}

func isURL(s string) bool {
	return strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://")
}

func (r *Resolver) Load(ctx context.Context, identifier string) LoadResult {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return Empty()
	}
	if cached, ok := r.loads.Get(identifier); ok {
		return cached
	}
	res := r.load(ctx, identifier)
	if res.Type != LoadError {
		r.loads.Set(identifier, res)
	}
	return res
}

func (r *Resolver) load(ctx context.Context, identifier string) LoadResult {
	if isURL(identifier) {
		for _, s := range r.ordered {
			if s.MatchesURL(identifier) {
				res, err := s.LoadURL(ctx, identifier)
				if err != nil {
					return LoadResult{Type: LoadError, Error: Classify(s.Name(), err)}
				}
				return res
			}
		}
		return LoadResult{Type: LoadError, Error: Fail(SeverityCommon, "", "no source handles this link", nil)}
	}
	if isrc, ok := isrcIdentifier(identifier); ok {
		lookup, supported := r.audio.(ISRCLookup)
		if !supported {
			return Empty()
		}
		t, err := lookup.ByISRC(ctx, isrc)
		if err != nil {
			return LoadResult{Type: LoadError, Error: Classify(r.audio.Name(), err)}
		}
		return Single(t)
	}
	if s, query, ok := r.byPrefix(identifier); ok {
		return r.search(ctx, s, query)
	}
	for _, alias := range SearchAliases {
		if query, ok := strings.CutPrefix(identifier, alias); ok {
			return r.search(ctx, r.audio, strings.TrimSpace(query))
		}
	}
	var last LoadResult = Empty()
	for _, name := range r.searchOrder {
		s, ok := r.sources[name]
		if !ok {
			continue
		}
		res := r.search(ctx, s, identifier)
		if res.Type == LoadSearch {
			return res
		}
		last = res
	}
	return last
}

func (r *Resolver) search(ctx context.Context, s Source, query string) LoadResult {
	if query == "" {
		return Empty()
	}
	tracks, err := s.Search(ctx, query)
	if err != nil {
		return LoadResult{Type: LoadError, Error: Classify(s.Name(), err)}
	}
	return Results(RankForQuery(query, tracks))
}

func (r *Resolver) Playable(ctx context.Context, t Track) (Track, error) {
	if r.audio == nil {
		return Track{}, Fail(SeverityFault, "", "no audio source is configured", nil)
	}
	if t.Source == r.audio.Name() {
		return t, nil
	}
	key := t.Key()
	if e, ok := r.mirrors.Get(key); ok {
		if !e.ok {
			return Track{}, Fail(SeverityCommon, t.Source, "no matching recording on "+r.audio.Name(), nil)
		}
		return e.track, nil
	}
	r.mu.Lock()
	if f, ok := r.inflight[key]; ok {
		r.mu.Unlock()
		select {
		case <-f.done:
			return f.track, f.err
		case <-ctx.Done():
			return Track{}, ctx.Err()
		}
	}
	f := &flight{done: make(chan struct{})}
	r.inflight[key] = f
	r.mu.Unlock()

	f.track, f.err = r.mirror(ctx, t)
	close(f.done)
	r.mu.Lock()
	delete(r.inflight, key)
	r.mu.Unlock()
	return f.track, f.err
}

func (r *Resolver) mirror(ctx context.Context, t Track) (Track, error) {
	key := t.Key()
	if t.ISRC == "" {
		if s, ok := r.sources[t.Source]; ok {
			if full, err := s.Track(ctx, t.Identifier); err == nil && full.ISRC != "" {
				t.ISRC = full.ISRC
				if t.DurationMs == 0 {
					t.DurationMs = full.DurationMs
				}
			}
		}
	}
	t.ISRC = NormalizeISRC(t.ISRC)
	if lookup, ok := r.audio.(ISRCLookup); ok && t.ISRC != "" {
		hit, err := lookup.ByISRC(ctx, t.ISRC)
		if err == nil && WithinDrift(t.DurationMs, hit.DurationMs) {
			r.mirrors.Set(key, mirrorEntry{track: hit, ok: true})
			return hit, nil
		}
		if err != nil && ctx.Err() != nil {
			return Track{}, ctx.Err()
		}
	}
	candidates, err := r.audio.Search(ctx, strings.TrimSpace(t.Title+" "+t.Author))
	if err != nil {
		return Track{}, Classify(r.audio.Name(), err)
	}
	hit, ok := BestMatch(t, candidates)
	if !ok {
		r.mirrors.SetFor(key, mirrorEntry{}, missCacheTTL)
		return Track{}, Fail(SeverityCommon, t.Source, fmt.Sprintf("no matching recording on %s", r.audio.Name()), nil)
	}
	r.mirrors.Set(key, mirrorEntry{track: hit, ok: true})
	return hit, nil
}

func (r *Resolver) Audio() Playable {
	return r.audio
}

func (r *Resolver) Enqueue(t Track) bool {
	key := t.Key()
	r.mu.Lock()
	if _, dup := r.warming[key]; dup {
		r.mu.Unlock()
		return true
	}
	if len(r.warming) >= prefetchPending {
		r.mu.Unlock()
		metrics.Prefetch.Inc("dropped")
		r.log.Warn("prefetch dropped, queue full", "track", key, "pending", prefetchPending)
		return false
	}
	r.warming[key] = struct{}{}
	r.mu.Unlock()
	metrics.Prefetch.Inc("queued")
	go func() {
		defer func() {
			r.mu.Lock()
			delete(r.warming, key)
			r.mu.Unlock()
		}()
		r.warmSlots <- struct{}{}
		defer func() { <-r.warmSlots }()
		ctx, cancel := context.WithTimeout(context.Background(), prefetchTimeout)
		defer cancel()
		r.Prefetch(ctx, t)
	}()
	return true
}

func (r *Resolver) Prefetch(ctx context.Context, t Track) {
	playable, err := r.Playable(ctx, t)
	if err != nil {
		metrics.Prefetch.Inc("failed")
		r.log.Warn("prefetch failed", "track", t.Key(), "error", err)
		return
	}
	r.audio.Warm(ctx, playable)
}
