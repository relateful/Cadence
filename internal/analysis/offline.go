package analysis

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/greed/cadence/internal/audio"
	"github.com/greed/cadence/internal/codec"
	"github.com/greed/cadence/internal/source"
)

const (
	offlineTimeout = 3 * time.Minute
	maxTrackLength = 20 * time.Minute
)

type Opener func(ctx context.Context, t source.Track) (source.Stream, error)

type job struct {
	track source.Track
	key   string
}

type Offline struct {
	registry *Registry
	open     Opener
	queue    chan job
	mu       sync.Mutex
	inFlight map[string]bool
	dropped  atomic.Int64
	done     atomic.Int64
	failed   atomic.Int64
	log      *slog.Logger
}

func NewOffline(registry *Registry, open Opener, workers, queueSize int, log *slog.Logger) *Offline {
	o := &Offline{registry: registry, open: open, queue: make(chan job, max(1, queueSize)), inFlight: map[string]bool{}, log: log.With("component", "analysis")}
	for i := 0; i < max(1, workers); i++ {
		go o.worker()
	}
	return o
}

type Stats struct {
	Queued  int   `json:"queued"`
	Dropped int64 `json:"dropped"`
	Done    int64 `json:"done"`
	Failed  int64 `json:"failed"`
}

func (o *Offline) Stats() Stats {
	return Stats{Queued: len(o.queue), Dropped: o.dropped.Load(), Done: o.done.Load(), Failed: o.failed.Load()}
}

func (o *Offline) Enqueue(t source.Track, force bool) bool {
	k := key(t.Source, t.Identifier)
	if !force && o.registry.Has(t.Source, t.Identifier) {
		return false
	}
	o.mu.Lock()
	if o.inFlight[k] {
		o.mu.Unlock()
		return false
	}
	o.inFlight[k] = true
	o.mu.Unlock()
	select {
	case o.queue <- job{track: t, key: k}:
		return true
	default:
		o.release(k)
		o.dropped.Add(1)
		return false
	}
}

func (o *Offline) release(k string) {
	o.mu.Lock()
	delete(o.inFlight, k)
	o.mu.Unlock()
}

func (o *Offline) worker() {
	for j := range o.queue {
		o.run(j)
	}
}

func (o *Offline) run(j job) {
	defer o.release(j.key)
	defer func() {
		if r := recover(); r != nil {
			o.failed.Add(1)
			o.log.Error("analysis panicked", "track", j.key, "panic", r, "stack", string(debug.Stack()))
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), offlineTimeout)
	defer cancel()
	fp, err := o.analyze(ctx, j.track)
	if err != nil {
		o.failed.Add(1)
		o.log.Warn("analysis failed", "track", j.key, "error", err)
		return
	}
	o.registry.Store(fp)
	o.done.Add(1)
}

func (o *Offline) analyze(ctx context.Context, t source.Track) (*Fingerprint, error) {
	stream, err := o.open(ctx, t)
	if err != nil {
		return nil, err
	}
	defer stream.Body.Close()
	pcm, err := codec.Decode(stream.Format, stream.Body)
	if err != nil {
		return nil, err
	}
	a := NewAnalyzer(t.Source, t.Identifier, nil)
	rs := audio.NewResampler(pcm.SampleRate(), 1)
	in := make([]int16, 8192)
	out := make([]float64, audio.FrameSamples)
	var frame audio.Frame
	pending := 0
	limit := int64(maxTrackLength / time.Millisecond * audio.SampleRate / 1000)
	for a.samples < limit {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		n, rerr := pcm.Read(in)
		rs.Write(in[:n])
		for {
			got := rs.Read(out[pending*2:])
			pending += got
			if pending < audio.FrameSamplesPerChannel {
				break
			}
			for i := range frame {
				frame[i] = audio.Clip(out[i])
			}
			a.Feed(&frame)
			pending = 0
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			return nil, fmt.Errorf("decode: %w", rerr)
		}
	}
	return a.Result(), nil
}
