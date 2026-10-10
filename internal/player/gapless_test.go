package player

import (
	"testing"
	"time"

	"github.com/greed/cadence/internal/audio"
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
