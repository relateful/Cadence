package player

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/greed/cadence/internal/audio"
	"github.com/greed/cadence/internal/source"
)

func count(rec *recorder, typ EventType) int {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	n := 0
	for _, e := range rec.events {
		if e.Type == typ {
			n++
		}
	}
	return n
}

func playIntoNext(t *testing.T, p *Player, rec *recorder) (aInst, bInst uint64, gaps int, out *audio.Frame) {
	t.Helper()
	aInst = p.Play(track("a"), 0, false)
	rec.wait(t, EventTrackStart)
	var err error
	if bInst, err = p.SetNext(track("b")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	out = &audio.Frame{}
	frame(t, p, out)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		info, ok := p.NextFrame(out)
		if !ok {
			gaps++
			continue
		}
		if info.Instance == bInst {
			return aInst, bInst, gaps, out
		}
	}
	t.Fatal("never reached the next track")
	return
}

func TestNextTrackStartsOnTheVeryNextFrame(t *testing.T) {
	p, rec := newTestPlayer(map[string]float64{"a": 0.5, "b": 3})
	defer p.Destroy()
	_, _, gaps, _ := playIntoNext(t, p, rec)
	if gaps != 0 {
		t.Fatalf("%d empty frames between tracks", gaps)
	}
	if e := rec.wait(t, EventTrackEnd); e.Reason != EndFinished || e.Track.Identifier != "a" {
		t.Fatalf("end %+v", e)
	}
	if count(rec, EventTrackStart) != 1 {
		t.Fatal("next track announced before the client adopted it")
	}
}

func TestPlayOfTheNextTrackAdoptsItWithoutRestarting(t *testing.T) {
	p, rec := newTestPlayer(map[string]float64{"a": 0.5, "b": 3})
	defer p.Destroy()
	_, bInst, _, out := playIntoNext(t, p, rec)
	for i := 0; i < 10; i++ {
		frame(t, p, out)
	}
	before := p.State().PositionMs
	if inst := p.Play(track("b"), 0, false); inst != bInst {
		t.Fatalf("adopt returned instance %d, want %d", inst, bInst)
	}
	if got := p.State().PositionMs; got < before {
		t.Fatalf("adopting restarted the track: %d -> %d", before, got)
	}
	deadline := time.Now().Add(time.Second)
	for count(rec, EventTrackStart) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if count(rec, EventTrackStart) != 2 {
		t.Fatal("adoption did not announce the track")
	}
}

func TestPlayOfAnotherTrackReplacesTheGaplessOne(t *testing.T) {
	p, rec := newTestPlayer(map[string]float64{"a": 0.5, "b": 3, "c": 3})
	defer p.Destroy()
	_, bInst, _, _ := playIntoNext(t, p, rec)
	if inst := p.Play(track("c"), 0, false); inst == bInst {
		t.Fatal("a different track was adopted")
	}
	if st := p.State(); st.Track == nil || st.Track.Identifier != "c" {
		t.Fatalf("state %+v", st.Track)
	}
}

func TestUnadoptedNextTrackIsAnnouncedWhenTheWindowCloses(t *testing.T) {
	old := adoptWindow
	adoptWindow = 50 * time.Millisecond
	defer func() { adoptWindow = old }()
	p, rec := newTestPlayer(map[string]float64{"a": 0.5, "b": 3})
	defer p.Destroy()
	_, _, _, out := playIntoNext(t, p, rec)
	time.Sleep(80 * time.Millisecond)
	frame(t, p, out)
	deadline := time.Now().Add(time.Second)
	for count(rec, EventTrackStart) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if count(rec, EventTrackStart) != 2 {
		t.Fatal("next track never announced")
	}
}

func TestAudibleOfTheGaplessTrackFollowsItsStart(t *testing.T) {
	p, rec := newTestPlayer(map[string]float64{"a": 0.5, "b": 3})
	defer p.Destroy()
	_, bInst, _, out := playIntoNext(t, p, rec)
	info, _ := p.NextFrame(out)
	p.Sent(info, time.Now())
	if count(rec, EventTrackAudible) > 1 {
		t.Fatal("audible sent for a track the client has not been told about")
	}
	p.Play(track("b"), 0, false)
	deadline := time.Now().Add(time.Second)
	for count(rec, EventTrackAudible) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	start, audible := -1, -1
	for i, e := range rec.events {
		if e.Instance != bInst {
			continue
		}
		if e.Type == EventTrackStart && start < 0 {
			start = i
		}
		if e.Type == EventTrackAudible && audible < 0 {
			audible = i
		}
	}
	if start < 0 || audible < 0 || audible < start {
		t.Fatalf("start at %d, audible at %d", start, audible)
	}
}

func TestFailedNextTrackIsNotSwappedIn(t *testing.T) {
	p, rec := newTestPlayer(map[string]float64{"a": 0.4})
	defer p.Destroy()
	p.Play(track("a"), 0, false)
	rec.wait(t, EventTrackStart)
	if _, err := p.SetNext(track("missing")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	var out audio.Frame
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !rec.has(EventTrackEnd) {
		p.NextFrame(&out)
	}
	if e := rec.wait(t, EventTrackEnd); e.Track.Identifier != "a" {
		t.Fatalf("end %+v", e)
	}
	if st := p.State(); st.Track != nil {
		t.Fatalf("a failed next track became active: %+v", st.Track)
	}
}

type gate struct{ release chan struct{} }

func (g gate) Playable(_ context.Context, t source.Track) (source.Track, error) {
	if t.Identifier == "slow" {
		<-g.release
	}
	return t, nil
}

func TestClearedNextIsNotRevivedByASlowPreload(t *testing.T) {
	rec := &recorder{}
	g := gate{release: make(chan struct{})}
	p := New(Config{GuildID: "1", BufferFrames: 200, Open: opener(map[string]float64{"a": 5, "slow": 5}), Resolve: g, OnEvent: rec.add, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	defer p.Destroy()
	p.Play(track("a"), 0, false)
	rec.wait(t, EventTrackStart)
	done := make(chan error, 1)
	go func() {
		_, err := p.SetNext(track("slow"))
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	p.ClearNext()
	close(g.release)
	if err := <-done; !errors.Is(err, ErrNextSuperseded) {
		t.Fatalf("slow preload: %v", err)
	}
	if st := p.State(); st.NextTrack != nil {
		t.Fatalf("cleared next track came back: %+v", st.NextTrack)
	}
}
