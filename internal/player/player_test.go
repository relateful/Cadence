package player

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/greed/cadence/internal/audio"
	"github.com/greed/cadence/internal/codec"
	"github.com/greed/cadence/internal/source"
)

type identity struct{}

func (identity) Playable(_ context.Context, t source.Track) (source.Track, error) {
	return t, nil
}

func tone(seconds float64, hz float64) []byte {
	n := int(seconds * audio.SampleRate)
	buf := make([]byte, n*4)
	for i := 0; i < n; i++ {
		v := int16(8000 * math.Sin(2*math.Pi*hz*float64(i)/audio.SampleRate))
		binary.LittleEndian.PutUint16(buf[4*i:], uint16(v))
		binary.LittleEndian.PutUint16(buf[4*i+2:], uint16(v))
	}
	return buf
}

func opener(lengths map[string]float64) Opener {
	return func(_ context.Context, t source.Track, _ int64) (source.Stream, error) {
		secs, ok := lengths[t.Identifier]
		if !ok {
			return source.Stream{}, errors.New("boom")
		}
		return source.Stream{Body: io.NopCloser(bytes.NewReader(tone(secs, 440))), Format: codec.FormatPCM48}, nil
	}
}

type recorder struct {
	mu     sync.Mutex
	events []Event
}

func (r *recorder) add(e Event) {
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
}

func (r *recorder) has(t EventType) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.events {
		if e.Type == t {
			return true
		}
	}
	return false
}

func (r *recorder) wait(t *testing.T, typ EventType) Event {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		for _, e := range r.events {
			if e.Type == typ {
				r.mu.Unlock()
				return e
			}
		}
		r.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no %s event", typ)
	return Event{}
}

func newTestPlayer(lengths map[string]float64) (*Player, *recorder) {
	rec := &recorder{}
	p := New(Config{GuildID: "1", BufferFrames: 200, Open: opener(lengths), Resolve: identity{}, OnEvent: rec.add, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	return p, rec
}

func frame(t *testing.T, p *Player, out *audio.Frame) FrameInfo {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if info, ok := p.NextFrame(out); ok {
			return info
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("no frame produced")
	return FrameInfo{}
}

func track(id string) source.Track {
	return source.Track{Source: "test", Identifier: id, Title: id}
}

func TestPositionAdvancesTwentyMsPerFrame(t *testing.T) {
	p, rec := newTestPlayer(map[string]float64{"a": 5})
	defer p.Destroy()
	p.Play(track("a"), 0, false)
	rec.wait(t, EventTrackStart)
	var out audio.Frame
	first := frame(t, p, &out)
	var last FrameInfo
	for i := 0; i < 50; i++ {
		last = frame(t, p, &out)
	}
	if got := last.PositionMs - first.PositionMs; math.Abs(got-1000) > 1 {
		t.Fatalf("50 frames advanced %vms, want 1000", got)
	}
}

func TestStartOffsetIsExact(t *testing.T) {
	p, rec := newTestPlayer(map[string]float64{"a": 5})
	defer p.Destroy()
	p.Play(track("a"), 1500, false)
	rec.wait(t, EventTrackStart)
	var out audio.Frame
	if info := frame(t, p, &out); math.Abs(info.PositionMs-1500) > 1 {
		t.Fatalf("first frame at %v, want 1500", info.PositionMs)
	}
}

func TestTimescaleMovesPositionFaster(t *testing.T) {
	p, rec := newTestPlayer(map[string]float64{"a": 8})
	defer p.Destroy()
	p.SetFilters(Filters{Timescale: &Timescale{Speed: 1.5, Pitch: 1, Rate: 1}})
	p.Play(track("a"), 0, false)
	rec.wait(t, EventTrackStart)
	var out audio.Frame
	first := frame(t, p, &out)
	var last FrameInfo
	for i := 0; i < 100; i++ {
		last = frame(t, p, &out)
	}
	if ratio := (last.PositionMs - first.PositionMs) / 2000; math.Abs(ratio-1.5) > 0.05 {
		t.Fatalf("position advanced %.3fx real time, want 1.5", ratio)
	}
	if r := p.State().Rate; r != 1.5 {
		t.Fatalf("state rate %v", r)
	}
}

func TestTrackEndsFinished(t *testing.T) {
	p, rec := newTestPlayer(map[string]float64{"a": 0.2})
	defer p.Destroy()
	p.Play(track("a"), 0, false)
	rec.wait(t, EventTrackStart)
	var out audio.Frame
	for i := 0; i < 200 && !rec.has(EventTrackEnd); i++ {
		p.NextFrame(&out)
		time.Sleep(time.Millisecond)
	}
	if e := rec.wait(t, EventTrackEnd); e.Reason != EndFinished {
		t.Fatalf("reason %s", e.Reason)
	}
}

func TestOpenFailureReportsException(t *testing.T) {
	p, rec := newTestPlayer(map[string]float64{})
	defer p.Destroy()
	p.Play(track("missing"), 0, false)
	e := rec.wait(t, EventTrackException)
	if e.Exception == nil || e.Exception.Cause != "boom" {
		t.Fatalf("exception %+v", e.Exception)
	}
	if e := rec.wait(t, EventTrackEnd); e.Reason != EndLoadFailed {
		t.Fatalf("reason %s", e.Reason)
	}
}

func TestAudibleFiresOnceAtFirstSend(t *testing.T) {
	p, rec := newTestPlayer(map[string]float64{"a": 5})
	defer p.Destroy()
	p.Play(track("a"), 0, false)
	rec.wait(t, EventTrackStart)
	if rec.has(EventTrackAudible) {
		t.Fatal("audible before any frame was sent")
	}
	var out audio.Frame
	now := time.Now()
	info := frame(t, p, &out)
	p.Sent(info, now)
	p.Sent(frame(t, p, &out), now.Add(20*time.Millisecond))
	e := rec.wait(t, EventTrackAudible)
	if e.SentAt != now.UnixMilli() || e.PositionMs != 0 {
		t.Fatalf("audible %+v", e)
	}
	rec.mu.Lock()
	count := 0
	for _, ev := range rec.events {
		if ev.Type == EventTrackAudible {
			count++
		}
	}
	rec.mu.Unlock()
	if count != 1 {
		t.Fatalf("%d audible events", count)
	}
}

func TestNoAudioWithoutVoiceMeansNoPositionDrift(t *testing.T) {
	p, rec := newTestPlayer(map[string]float64{"a": 5})
	defer p.Destroy()
	p.Play(track("a"), 0, false)
	rec.wait(t, EventTrackStart)
	time.Sleep(100 * time.Millisecond)
	if s := p.State(); s.PositionMs != 0 || s.Sounding {
		t.Fatalf("position moved with no sender: %+v", s)
	}
}

func TestArmedCrossfadePromotesAtCue(t *testing.T) {
	p, rec := newTestPlayer(map[string]float64{"a": 6, "b": 6})
	defer p.Destroy()
	p.Play(track("a"), 0, false)
	rec.wait(t, EventTrackStart)
	if _, err := p.LoadDeckB(track("b"), 2000); err != nil {
		t.Fatal(err)
	}
	at := int64(400)
	if err := p.StartCrossfade(CrossfadeRequest{DurationMs: 200, Curve: "LINEAR", AtPositionMs: &at}); err != nil {
		t.Fatal(err)
	}
	var out audio.Frame
	for i := 0; i < 60; i++ {
		frame(t, p, &out)
	}
	started := rec.wait(t, EventMixStarted)
	if started.Into == nil || started.Into.Identifier != "b" {
		t.Fatalf("mix into %+v", started.Into)
	}
	promoted := rec.wait(t, EventMixPromoted)
	if promoted.IntoMs < 2000 {
		t.Fatalf("deck b position %d, want >= 2000", promoted.IntoMs)
	}
	if s := p.State(); s.Track == nil || s.Track.Identifier != "b" || s.Mixing {
		t.Fatalf("state after promote %+v", s)
	}
}

func TestCrossfadeWithoutDeckBFails(t *testing.T) {
	p, _ := newTestPlayer(map[string]float64{"a": 5})
	defer p.Destroy()
	if err := p.StartCrossfade(CrossfadeRequest{DurationMs: 100}); !errors.Is(err, ErrNoDeckB) {
		t.Fatalf("err %v", err)
	}
}
