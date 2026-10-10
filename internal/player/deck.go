package player

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/greed/cadence/internal/audio"
	"github.com/greed/cadence/internal/codec"
	"github.com/greed/cadence/internal/source"
)

const (
	chunkSamples      = 4096
	stuckAfter        = 10 * time.Second
	streamIdleTimeout = 30 * time.Second
	decodeChunkSize   = chunkSamples * audio.Channels
)

type chunk struct {
	samples []int16
	start   int64
	rate    int
}

type Opener func(ctx context.Context, t source.Track, startMs int64) (source.Stream, error)

type Tap interface {
	Feed(frame *audio.Frame)
	Finish()
}

type Deck struct {
	Track    source.Track
	Playable source.Track
	queue    chan chunk
	cancel   context.CancelFunc
	done     chan struct{}
	err      atomic.Pointer[source.Failure]
	ended    atomic.Bool
	startMs  int64

	resampler *audio.Resampler
	stretch   *audio.Stretcher
	rate      int
	floatBuf  []float64
	posBuf    []float64
	hop       []float64

	filters    Filters
	fx         effects
	ramp       filterRamp
	tempoRatio float64
	positionMs float64
	produced   int64
	starvedAt  time.Time
	stuckSent  bool
	tap        Tap
	norm       float64
	measured   bool
	mu         sync.Mutex
	log        *slog.Logger
}

func newDeck(t, playable source.Track, bufferFrames int, log *slog.Logger) *Deck {
	chunks := max(4, bufferFrames*audio.FrameSamplesPerChannel/chunkSamples)
	return &Deck{
		Track:      t,
		Playable:   playable,
		queue:      make(chan chunk, chunks),
		done:       make(chan struct{}),
		stretch:    audio.NewStretcher(),
		hop:        make([]float64, audio.FrameSamples),
		tempoRatio: 1,
		norm:       1,
		filters:    DefaultFilters(),
		log:        log,
	}
}

func (d *Deck) start(parent context.Context, open Opener, startMs int64) {
	ctx, cancel := context.WithCancel(parent)
	d.cancel = cancel
	d.startMs = startMs
	d.positionMs = float64(startMs)
	go d.produce(ctx, open, startMs)
}

func (d *Deck) fail(f *source.Failure) {
	d.err.CompareAndSwap(nil, f)
}

func (d *Deck) Failure() *source.Failure {
	return d.err.Load()
}

type idleReader struct {
	r     io.Reader
	timer *time.Timer
}

func (i idleReader) Read(p []byte) (int, error) {
	n, err := i.r.Read(p)
	if n > 0 {
		i.timer.Reset(streamIdleTimeout)
	}
	return n, err
}

func (d *Deck) produce(parent context.Context, open Opener, startMs int64) {
	defer close(d.done)
	defer close(d.queue)
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	var stalled atomic.Bool
	timer := time.AfterFunc(streamIdleTimeout, func() {
		stalled.Store(true)
		cancel()
	})
	defer timer.Stop()
	stream, err := open(ctx, d.Playable, startMs)
	if err != nil {
		if parent.Err() == nil {
			d.fail(source.Classify(d.Playable.Source, err))
		}
		return
	}
	defer stream.Body.Close()
	defer func() {
		if stalled.Load() && parent.Err() == nil {
			d.fail(source.Fail(source.SeverityCommon, d.Playable.Source, "stream stalled", fmt.Errorf("no data for %s", streamIdleTimeout)))
		}
	}()
	pcm, err := codec.Decode(stream.Format, idleReader{r: stream.Body, timer: timer})
	if err != nil {
		d.fail(source.Fail(source.SeverityFault, d.Playable.Source, "decode failed", err))
		return
	}
	rate := pcm.SampleRate()
	skip := startMs * int64(rate) / 1000
	index := stream.StartSample
	for {
		buf := make([]int16, decodeChunkSize)
		n, rerr := pcm.Read(buf)
		frames := int64(n / audio.Channels)
		if frames > 0 {
			c := chunk{samples: buf[:frames*audio.Channels], start: index, rate: rate}
			if index+frames > skip {
				if index < skip {
					cut := skip - index
					c.samples = c.samples[cut*audio.Channels:]
					c.start = skip
				}
				timer.Stop()
				select {
				case d.queue <- c:
					timer.Reset(streamIdleTimeout)
				case <-ctx.Done():
					return
				}
			}
			index += frames
		}
		if rerr != nil {
			if !errors.Is(rerr, io.EOF) && ctx.Err() == nil {
				d.fail(source.Fail(source.SeverityCommon, d.Playable.Source, "stream interrupted", rerr))
			}
			return
		}
		if ctx.Err() != nil {
			return
		}
	}
}

func (d *Deck) SetFilters(f Filters) {
	d.mu.Lock()
	d.filters = f
	d.mu.Unlock()
}

func (d *Deck) SetTempoRatio(r float64) {
	d.mu.Lock()
	if r <= 0 {
		r = 1
	}
	d.tempoRatio = r
	d.mu.Unlock()
}

func (d *Deck) TempoRatio() float64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.tempoRatio
}

func (d *Deck) attachTap(t Tap) {
	d.mu.Lock()
	d.tap = t
	d.mu.Unlock()
}

func (d *Deck) PositionMs() int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return int64(d.positionMs)
}

func (d *Deck) Ended() bool {
	return d.ended.Load()
}

func (d *Deck) Stuck() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.starvedAt.IsZero() || d.stuckSent || time.Since(d.starvedAt) < stuckAfter {
		return false
	}
	d.stuckSent = true
	return true
}

func (d *Deck) feed() bool {
	select {
	case c, ok := <-d.queue:
		if !ok {
			return false
		}
		if d.resampler == nil || c.rate != d.rate {
			d.rate = c.rate
			d.resampler = audio.NewResampler(c.rate, d.ramp.resampleFactor(d.filters))
			d.resampler.Reset(c.start)
		}
		d.resampler.Write(c.samples)
		return true
	default:
		return false
	}
}

func (d *Deck) Read(out *audio.Frame) (float64, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ended.Load() {
		return d.positionMs, false
	}
	d.ramp.advance(d.filters)
	d.stretch.SetTempo(d.ramp.tempoFactor(d.filters) * d.tempoRatio)
	for {
		if pos, ok := d.stretch.Next(d.hop); ok {
			d.emit(out, pos)
			return d.positionMs, true
		}
		if d.resampler != nil {
			d.resampler.Configure(d.rate, d.ramp.resampleFactor(d.filters))
			if d.pump() {
				continue
			}
		}
		if d.feed() {
			continue
		}
		select {
		case <-d.done:
			if len(d.queue) == 0 {
				d.ended.Store(true)
				if d.tap != nil {
					d.tap.Finish()
					d.tap = nil
				}
				return d.positionMs, false
			}
			continue
		default:
		}
		if d.starvedAt.IsZero() {
			d.starvedAt = time.Now()
		}
		return d.positionMs, false
	}
}

func (d *Deck) pump() bool {
	avail := d.resampler.Available()
	if avail <= 0 {
		return false
	}
	n := min(avail, 4096)
	if cap(d.floatBuf) < 2*n {
		d.floatBuf = make([]float64, 2*n)
		d.posBuf = make([]float64, n)
	}
	first := d.resampler.Position()
	step := d.resampler.Step()
	got := d.resampler.Read(d.floatBuf[:2*n])
	for i := 0; i < got; i++ {
		d.posBuf[i] = (first + float64(i)*step) * 1000 / float64(d.rate)
	}
	d.stretch.Write(d.floatBuf[:2*got], d.posBuf[:got])
	return got > 0
}

func (d *Deck) emit(out *audio.Frame, posMs float64) {
	for i := range out {
		out[i] = audio.Clip(d.hop[i])
	}
	d.fx.apply(d.ramp.effective(), d.ramp.wet, out)
	d.positionMs = posMs
	d.produced++
	d.starvedAt = time.Time{}
	d.stuckSent = false
	if d.tap != nil {
		d.tap.Feed(out)
	}
}

func (d *Deck) Close() {
	if d.cancel != nil {
		d.cancel()
	}
	d.mu.Lock()
	if d.tap != nil {
		d.tap = nil
	}
	d.mu.Unlock()
}
