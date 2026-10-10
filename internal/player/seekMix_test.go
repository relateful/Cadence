package player

import (
	"testing"

	"github.com/greed/cadence/internal/audio"
)

func TestSeekDuringArmedMixDropsTheMixAndSeeks(t *testing.T) {
	p, rec := newTestPlayer(map[string]float64{"a": 6, "b": 6})
	defer p.Destroy()
	p.Play(track("a"), 0, false)
	rec.wait(t, EventTrackStart)
	if _, err := p.LoadDeckB(track("b"), 0); err != nil {
		t.Fatal(err)
	}
	at := int64(4000)
	if err := p.StartCrossfade(CrossfadeRequest{DurationMs: 200, Curve: "LINEAR", AtPositionMs: &at}); err != nil {
		t.Fatal(err)
	}
	if err := p.SeekTo(1000); err != nil {
		t.Fatalf("seek while armed should succeed, got %v", err)
	}
	rec.wait(t, EventMixDropped)
	var out audio.Frame
	info := frame(t, p, &out)
	if info.PositionMs < 990 || info.PositionMs > 1100 {
		t.Fatalf("position after seek %v, want about 1000", info.PositionMs)
	}
	if s := p.State(); s.Track == nil || s.Track.Identifier != "a" || s.Mixing {
		t.Fatalf("state after dropping the mix %+v", s)
	}
}

func TestSeekDuringRunningFadeLandsOnTheIncomingTrack(t *testing.T) {
	p, rec := newTestPlayer(map[string]float64{"a": 6, "b": 6})
	defer p.Destroy()
	p.Play(track("a"), 0, false)
	rec.wait(t, EventTrackStart)
	if _, err := p.LoadDeckB(track("b"), 0); err != nil {
		t.Fatal(err)
	}
	if err := p.StartCrossfade(CrossfadeRequest{DurationMs: 3000, Curve: "LINEAR"}); err != nil {
		t.Fatal(err)
	}
	var out audio.Frame
	for i := 0; i < 10; i++ {
		frame(t, p, &out)
	}
	rec.wait(t, EventMixStarted)
	if err := p.SeekTo(2500); err != nil {
		t.Fatalf("seek while fading should succeed, got %v", err)
	}
	rec.wait(t, EventMixPromoted)
	info := frame(t, p, &out)
	if s := p.State(); s.Track == nil || s.Track.Identifier != "b" || s.Mixing {
		t.Fatalf("state after seek %+v", s)
	}
	if info.PositionMs < 2490 || info.PositionMs > 2600 {
		t.Fatalf("incoming track position %v, want about 2500", info.PositionMs)
	}
}

func TestSecondFadePromotesTheRunningIncomingDeck(t *testing.T) {
	p, rec := newTestPlayer(map[string]float64{"a": 6, "b": 6, "c": 6})
	defer p.Destroy()
	p.Play(track("a"), 0, false)
	rec.wait(t, EventTrackStart)
	if _, err := p.LoadDeckB(track("b"), 0); err != nil {
		t.Fatal(err)
	}
	if err := p.StartCrossfade(CrossfadeRequest{DurationMs: 3000, Curve: "LINEAR"}); err != nil {
		t.Fatal(err)
	}
	var out audio.Frame
	for i := 0; i < 5; i++ {
		frame(t, p, &out)
	}
	if _, err := p.LoadDeckB(track("c"), 0); err != nil {
		t.Fatal(err)
	}
	if err := p.StartCrossfade(CrossfadeRequest{DurationMs: 3000, Curve: "LINEAR"}); err != nil {
		t.Fatal(err)
	}
	rec.wait(t, EventMixPromoted)
	s := p.State()
	if s.Track == nil || s.Track.Identifier != "b" || !s.Mixing {
		t.Fatalf("state after second fade %+v", s)
	}
}

func TestCancelPendingReportsTheDroppedMix(t *testing.T) {
	p, rec := newTestPlayer(map[string]float64{"a": 6, "b": 6})
	defer p.Destroy()
	p.Play(track("a"), 0, false)
	rec.wait(t, EventTrackStart)
	if _, err := p.LoadDeckB(track("b"), 0); err != nil {
		t.Fatal(err)
	}
	p.CancelPending()
	e := rec.wait(t, EventMixDropped)
	if e.Track == nil || e.Track.Identifier != "b" {
		t.Fatalf("dropped event names %+v, want b", e.Track)
	}
}
