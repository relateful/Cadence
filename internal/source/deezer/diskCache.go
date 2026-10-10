package deezer

import (
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/greed/cadence/internal/audiocache"
	"github.com/greed/cadence/internal/cache"
	"github.com/greed/cadence/internal/metrics"
)

const (
	playsToCache = 2
	playWindow   = 7 * 24 * time.Hour
	playEntries  = 200_000
)

type diskCache struct {
	store *audiocache.Store
	plays *cache.LRU[string, int]
	mu    sync.Mutex
}

func (d *Deezer) UseAudioCache(store *audiocache.Store) {
	d.disk = &diskCache{store: store, plays: cache.New[string, int](playEntries, playWindow)}
}

func cacheKey(trackID, format string) string {
	return trackID + "-" + strings.ToLower(format)
}

func (d *Deezer) openDisk(trackID string) (io.ReadCloser, bool) {
	if d.disk == nil {
		return nil, false
	}
	for _, f := range d.formats {
		key := cacheKey(trackID, f.Format)
		if !d.disk.store.Has(key) {
			continue
		}
		if file, _, ok := d.disk.store.Get(key); ok {
			return file, true
		}
	}
	metrics.AudioCache.Inc("miss")
	return nil, false
}

func (c *diskCache) countPlay(trackID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n, _ := c.plays.Get(trackID)
	n++
	c.plays.Set(trackID, n)
	return n
}

func (d *Deezer) teeToDisk(trackID string, link mediaLink, raw io.Reader, closer io.Closer, total func() int64) (io.Reader, io.Closer) {
	if d.disk == nil || link.Format == "" {
		return raw, closer
	}
	if d.disk.countPlay(trackID) < playsToCache {
		return raw, closer
	}
	key := cacheKey(trackID, link.Format)
	w, err := d.disk.store.Create(key, 0)
	if err != nil {
		metrics.AudioCache.Inc("write_failed")
		d.log.Warn("audio cache write could not start", "track", trackID, "error", err)
		return raw, closer
	}
	t := &cacheTee{src: raw, closer: closer, w: w, total: total, log: d.log.With("track", trackID, "key", key)}
	return t, t
}

type cacheTee struct {
	src    io.Reader
	closer io.Closer
	total  func() int64
	log    *slog.Logger

	mu      sync.Mutex
	w       *audiocache.Writer
	written int64
}

func (t *cacheTee) Read(p []byte) (int, error) {
	n, err := t.src.Read(p)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.w == nil {
		return n, err
	}
	if n > 0 {
		if _, werr := t.w.Write(p[:n]); werr != nil {
			t.abortLocked("write", werr)
			return n, err
		}
		t.written += int64(n)
	}
	switch {
	case errors.Is(err, io.EOF):
		t.commitLocked()
	case err != nil:
		t.abortLocked("stream", err)
	}
	return n, err
}

func (t *cacheTee) commitLocked() {
	w := t.w
	t.w = nil
	if expected := t.total(); expected > 0 && expected != t.written {
		w.Abort()
		metrics.AudioCache.Inc("incomplete")
		t.log.Warn("audio cache skipped an incomplete download", "written", t.written, "expected", expected)
		return
	}
	if err := w.Commit(); err != nil {
		metrics.AudioCache.Inc("write_failed")
		t.log.Warn("audio cache commit failed", "error", err)
		return
	}
	t.log.Debug("track cached to disk", "bytes", t.written)
}

func (t *cacheTee) abortLocked(stage string, err error) {
	t.w.Abort()
	t.w = nil
	if stage == "write" {
		metrics.AudioCache.Inc("write_failed")
		t.log.Warn("audio cache write failed, playback continues uncached", "error", err)
	}
}

func (t *cacheTee) Close() error {
	t.mu.Lock()
	if t.w != nil {
		t.w.Abort()
		t.w = nil
	}
	t.mu.Unlock()
	return t.closer.Close()
}
