package audio

import (
	"math"
	"testing"
)

func TestCrossfadeEqualPowerEndsFullyIn(t *testing.T) {
	c := NewCrossfade(100, CurveEqualPower)
	for !c.Finished() {
		c.Advance()
	}
	if math.Abs(c.InGain-1) > 1e-9 || math.Abs(c.OutGain) > 1e-9 {
		t.Fatalf("gains at end: out=%v in=%v", c.OutGain, c.InGain)
	}
	if c.DurationMs() != 100 {
		t.Fatalf("duration %d", c.DurationMs())
	}
}

func TestCrossfadeEqualPowerKeepsPower(t *testing.T) {
	c := NewCrossfade(400, CurveEqualPower)
	for !c.Finished() {
		c.Advance()
		if p := c.OutGain*c.OutGain + c.InGain*c.InGain; math.Abs(p-1) > 1e-9 {
			t.Fatalf("power %v", p)
		}
	}
}

func TestParseCurve(t *testing.T) {
	if c, _ := ParseCurve("linear"); c != CurveLinear {
		t.Fatal("linear")
	}
	if c, _ := ParseCurve(""); c != CurveEqualPower {
		t.Fatal("default")
	}
	if _, err := ParseCurve("bounce"); err == nil {
		t.Fatal("unknown curve accepted")
	}
}

func TestMixClips(t *testing.T) {
	var a, b, out Frame
	a[0], b[0] = 30000, 30000
	a[1], b[1] = -30000, -30000
	Mix(&a, 1, &b, 1, &out)
	if out[0] != 32767 || out[1] != -32768 {
		t.Fatalf("clip %d %d", out[0], out[1])
	}
}

func TestProfileBassSwap(t *testing.T) {
	at := 0.5
	p := TransitionProfile{BassSwapAt: &at, BassSwapRamp: 0.1}
	if p.OutgoingBassGain(0.4) != 1 || p.IncomingBassGain(0.4) != 0 {
		t.Fatal("before swap")
	}
	if p.OutgoingBassGain(0.7) != 0 || p.IncomingBassGain(0.7) != 1 {
		t.Fatal("after swap")
	}
	if g := p.OutgoingBassGain(0.55); math.Abs(g-0.5) > 1e-9 {
		t.Fatalf("mid ramp %v", g)
	}
	if (TransitionProfile{}).SwapsBass() {
		t.Fatal("plain swaps bass")
	}
}

func sine(rate, hz float64, seconds float64) []int16 {
	n := int(rate * seconds)
	out := make([]int16, 2*n)
	for i := 0; i < n; i++ {
		v := int16(10000 * math.Sin(2*math.Pi*hz*float64(i)/rate))
		out[2*i], out[2*i+1] = v, v
	}
	return out
}

func zeroCrossings(samples []float64) int {
	n := 0
	for i := 2; i < len(samples); i += 2 {
		if (samples[i-2] < 0) != (samples[i] < 0) {
			n++
		}
	}
	return n
}

func TestResamplerKeepsPitchAndRate(t *testing.T) {
	r := NewResampler(44100, 1)
	r.Write(sine(44100, 441, 1))
	out := make([]float64, 2*SampleRate)
	n := r.Read(out)
	if n < SampleRate-200 || n > SampleRate {
		t.Fatalf("produced %d frames", n)
	}
	hz := float64(zeroCrossings(out[:2*n])) / 2 / (float64(n) / SampleRate)
	if math.Abs(hz-441) > 3 {
		t.Fatalf("frequency %v", hz)
	}
	if math.Abs(r.Position()-float64(n)*44100/SampleRate) > 1 {
		t.Fatalf("position %v", r.Position())
	}
}

func TestStretcherTracksSourcePosition(t *testing.T) {
	s := NewStretcher()
	s.SetTempo(1.25)
	src := sine(SampleRate, 440, 4)
	samples := make([]float64, len(src))
	positions := make([]float64, len(src)/2)
	for i := range src {
		samples[i] = float64(src[i])
	}
	for i := range positions {
		positions[i] = float64(i)
	}
	s.Write(samples, positions)
	out := make([]float64, 2*stretchHop)
	var last float64
	hops := 0
	for {
		p, ok := s.Next(out)
		if !ok {
			break
		}
		last = p
		hops++
	}
	elapsed := float64(hops * stretchHop)
	if ratio := last / elapsed; math.Abs(ratio-1.25) > 0.05 {
		t.Fatalf("source advanced %.3fx of output, want 1.25", ratio)
	}
}

func TestStretcherPassthroughIsExact(t *testing.T) {
	s := NewStretcher()
	src := []float64{1, 2, 3, 4}
	big := make([]float64, 2*stretchHop)
	copy(big, src)
	pos := make([]float64, stretchHop)
	s.Write(big, pos)
	out := make([]float64, 2*stretchHop)
	if _, ok := s.Next(out); !ok || out[0] != 1 || out[3] != 4 {
		t.Fatalf("passthrough changed audio: %v", out[:4])
	}
}
