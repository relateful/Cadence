package player

import (
	"io"
	"log/slog"
	"math"
	"sync/atomic"
	"testing"

	"github.com/greed/cadence/internal/audio"
	"github.com/greed/cadence/internal/source"
)

func near(a, b float64) bool {
	return math.Abs(a-b) < 1e-3
}

func TestNormalizationGain(t *testing.T) {
	cases := []struct {
		name string
		in   Loudness
		want float64
	}{
		{"quiet track is boosted", Loudness{IntegratedLufs: -18}, math.Pow(10, 4.0/20)},
		{"loud track is cut", Loudness{IntegratedLufs: -8}, math.Pow(10, -6.0/20)},
		{"boost is capped at 6 dB", Loudness{IntegratedLufs: -40}, math.Pow(10, 6.0/20)},
		{"cut is capped at 12 dB", Loudness{IntegratedLufs: 5}, math.Pow(10, -12.0/20)},
		{"boost never pushes the peak past -1 dBFS", Loudness{IntegratedLufs: -20, PeakAmplitude: 0.8}, normPeakCeil / 0.8},
		{"silence or a failed measurement is left alone", Loudness{IntegratedLufs: math.Inf(-1)}, 1},
	}
	for _, c := range cases {
		if got := NormalizationGain(c.in, -14); !near(got, c.want) {
			t.Errorf("%s: got %.4f want %.4f", c.name, got, c.want)
		}
	}
}

func rms(f *audio.Frame) float64 {
	s := 0.0
	for _, v := range f {
		s += float64(v) * float64(v)
	}
	return math.Sqrt(s / float64(len(f)))
}

func levelOf(t *testing.T, lookup LoudnessLookup, unmeasured func(source.Track)) float64 {
	t.Helper()
	rec := &recorder{}
	p := New(Config{GuildID: "1", BufferFrames: 200, Open: opener(map[string]float64{"a": 3}), Resolve: identity{}, OnEvent: rec.add, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Loudness: lookup, OnUnmeasured: unmeasured, TargetLufs: -14})
	defer p.Destroy()
	p.Play(track("a"), 0, false)
	rec.wait(t, EventTrackStart)
	var out audio.Frame
	for i := 0; i < 10; i++ {
		frame(t, p, &out)
	}
	return rms(&out)
}

func TestMeasuredTrackIsLevelledAndUnmeasuredIsQueued(t *testing.T) {
	var queued atomic.Int32
	unity := levelOf(t, func(source.Track, source.Track) (Loudness, bool) { return Loudness{}, false }, func(source.Track) { queued.Add(1) })
	if queued.Load() != 1 {
		t.Fatalf("unmeasured track queued %d times", queued.Load())
	}
	boosted := levelOf(t, func(source.Track, source.Track) (Loudness, bool) { return Loudness{IntegratedLufs: -20, PeakAmplitude: 0.25}, true }, nil)
	if ratio := boosted / unity; math.Abs(ratio-math.Pow(10, 6.0/20)) > 0.05 {
		t.Fatalf("boost ratio %.3f, want about 2.0", ratio)
	}
}
