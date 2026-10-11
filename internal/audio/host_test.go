package audio

import (
	"math"
	"testing"
)

var shape = HostShape{VoiceMs: 6000, OutFadeMs: 2500, LeadMs: 1800, RampMs: 1200, Duck: DuckGain(9)}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestDuckGainIsInDecibels(t *testing.T) {
	if g := DuckGain(9); math.Abs(g-0.3548) > 1e-3 {
		t.Fatalf("DuckGain(9) = %v, want about 0.355", g)
	}
	if DuckGain(-9) != DuckGain(9) {
		t.Fatal("the sign of the duck must not matter")
	}
}

func TestOutgoingFadesOutUnderTheStartOfTheVoice(t *testing.T) {
	if !near(shape.OutGain(0), 1) {
		t.Fatal("outgoing starts at full level")
	}
	if g := shape.OutGain(1250); g <= 0 || g >= 1 {
		t.Fatalf("mid-fade gain = %v", g)
	}
	if shape.OutGain(2500) != 0 || shape.OutGain(4000) != 0 {
		t.Fatal("outgoing is silent once its fade ends")
	}
	if (HostShape{}).OutGain(0) != 0 {
		t.Fatal("no fade length means the outgoing track stops at once")
	}
}

func TestIncomingIsHeldUntilShortlyBeforeTheVoiceEnds(t *testing.T) {
	if shape.InOpenMs() != 4200 {
		t.Fatalf("InOpenMs = %d, want 4200", shape.InOpenMs())
	}
	if shape.InOpen(4199) || shape.InGain(4199) != 0 {
		t.Fatal("incoming must be held and silent before its window")
	}
	if !shape.InOpen(4200) {
		t.Fatal("incoming opens at voice end minus lead")
	}
}

func TestIncomingRisesToTheDuckUnderTheVoiceThenToFull(t *testing.T) {
	if g := shape.InGain(5000); g <= 0 || g >= shape.Duck {
		t.Fatalf("under the last words gain = %v, want between 0 and the duck", g)
	}
	if !near(shape.InGain(6000), shape.Duck) {
		t.Fatalf("at the end of the voice gain = %v, want the duck %v", shape.InGain(6000), shape.Duck)
	}
	if g := shape.InGain(6600); g <= shape.Duck || g >= 1 {
		t.Fatalf("during the ramp gain = %v", g)
	}
	if shape.InGain(7200) != 1 || shape.TotalMs() != 7200 {
		t.Fatal("incoming reaches full level when the ramp ends")
	}
}

func TestGainsNeverStep(t *testing.T) {
	prevIn, prevOut := shape.InGain(0), shape.OutGain(0)
	for ms := int64(FrameMs); ms <= shape.TotalMs(); ms += FrameMs {
		in, out := shape.InGain(ms), shape.OutGain(ms)
		if math.Abs(in-prevIn) > 0.06 || math.Abs(out-prevOut) > 0.06 {
			t.Fatalf("gain jumped at %dms: in %v→%v out %v→%v", ms, prevIn, in, prevOut, out)
		}
		prevIn, prevOut = in, out
	}
}

func TestALeadLongerThanTheVoiceOpensAtOnce(t *testing.T) {
	s := HostShape{VoiceMs: 1000, LeadMs: 5000, RampMs: 500, Duck: 0.5}
	if s.InOpenMs() != 0 || !s.InOpen(0) {
		t.Fatal("the window cannot open before the voice starts")
	}
}

func TestBusSumsWithoutClippingBetweenSources(t *testing.T) {
	var a, b, out Frame
	for i := range a {
		a[i], b[i] = 30000, -30000
	}
	var bus Bus
	bus.Add(&a, 1, 1)
	bus.Add(&b, 1, 1)
	bus.Write(&out)
	if out[0] != 0 {
		t.Fatalf("summed in float the two cancel, got %d", out[0])
	}
	bus.Reset()
	bus.Add(&a, 0, 0)
	bus.Write(&out)
	if out[0] != 0 {
		t.Fatal("a silent add contributes nothing")
	}
}

func TestBusRampsPerSample(t *testing.T) {
	var src, out Frame
	for i := range src {
		src[i] = 10000
	}
	var bus Bus
	bus.Add(&src, 0, 1)
	bus.Write(&out)
	if out[0] >= out[len(out)-1] || out[len(out)-1] != 10000 {
		t.Fatalf("ramp must rise across the frame, first %d last %d", out[0], out[len(out)-1])
	}
}
