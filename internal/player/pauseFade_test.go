package player

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/greed/cadence/internal/audio"
)

func fadingPlayer(fade time.Duration) (*Player, *recorder) {
	rec := &recorder{}
	p := New(Config{GuildID: "1", BufferFrames: 200, PauseFade: fade, Open: opener(map[string]float64{"a": 5}), Resolve: identity{}, OnEvent: rec.add, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	return p, rec
}

func TestPauseFadesOutThenStops(t *testing.T) {
	p, rec := fadingPlayer(100 * time.Millisecond)
	defer p.Destroy()
	p.Play(track("a"), 0, false)
	rec.wait(t, EventTrackStart)
	var out audio.Frame
	frame(t, p, &out)
	full := out.Peak()
	p.Pause(true)
	if !p.State().Paused {
		t.Fatal("state must report paused as soon as the pause is requested")
	}
	prev := full + 1
	frames := 0
	for {
		if _, ok := p.NextFrame(&out); !ok {
			break
		}
		frames++
		if peak := out.Peak(); peak >= prev {
			t.Fatalf("fade frame %d peak %d did not fall below %d", frames, peak, prev)
		} else {
			prev = peak
		}
		if frames > 10 {
			t.Fatal("fade-out never finished")
		}
	}
	tail := out[len(out)-4:]
	for _, s := range tail {
		if int(s) > full/100 || int(s) < -full/100 {
			t.Fatalf("fade-out ended on sample %d, want silence", s)
		}
	}
	if frames != 5 {
		t.Fatalf("fade-out took %d frames, want 5", frames)
	}
	if _, ok := p.NextFrame(&out); ok {
		t.Fatal("a paused player kept producing frames after its fade-out")
	}
}

func TestResumeFadesIn(t *testing.T) {
	p, rec := fadingPlayer(100 * time.Millisecond)
	defer p.Destroy()
	p.Play(track("a"), 0, false)
	rec.wait(t, EventTrackStart)
	var out audio.Frame
	frame(t, p, &out)
	full := out.Peak()
	p.Pause(true)
	for i := 0; i < 10; i++ {
		p.NextFrame(&out)
	}
	p.Pause(false)
	frame(t, p, &out)
	if out.Peak() >= full/2 {
		t.Fatalf("first frame after resume peaked at %d of %d, want a fade-in", out.Peak(), full)
	}
	for i := 0; i < 5; i++ {
		frame(t, p, &out)
	}
	if out.Peak() < full*9/10 {
		t.Fatalf("after the fade-in the peak is %d of %d", out.Peak(), full)
	}
}

func TestResumeDuringFadeOutReversesIt(t *testing.T) {
	p, rec := fadingPlayer(200 * time.Millisecond)
	defer p.Destroy()
	p.Play(track("a"), 0, false)
	rec.wait(t, EventTrackStart)
	var out audio.Frame
	frame(t, p, &out)
	p.Pause(true)
	p.NextFrame(&out)
	p.NextFrame(&out)
	p.Pause(false)
	low := p.env
	if low <= 0 || low >= 1 {
		t.Fatalf("expected to be mid fade-out, envelope %v", low)
	}
	frame(t, p, &out)
	if p.env <= low {
		t.Fatalf("resume mid fade-out did not turn the fade around: %v after %v", p.env, low)
	}
}

func TestNoFadeConfiguredPausesImmediately(t *testing.T) {
	p, rec := fadingPlayer(0)
	defer p.Destroy()
	p.Play(track("a"), 0, false)
	rec.wait(t, EventTrackStart)
	var out audio.Frame
	frame(t, p, &out)
	p.Pause(true)
	if _, ok := p.NextFrame(&out); ok {
		t.Fatal("without CADENCE_PAUSE_FADE_MS a pause must stop on the next frame")
	}
}
