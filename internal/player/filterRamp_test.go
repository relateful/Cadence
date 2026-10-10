package player

import (
	"math"
	"testing"

	"github.com/greed/cadence/internal/audio"
)

func framesFor(ms int) int { return ms / audio.FrameMs }

func TestFilterRampStartsAtTheFirstFiltersWithoutGliding(t *testing.T) {
	var r filterRamp
	f := Filters{Timescale: &Timescale{Speed: 1.3, Pitch: 1.3, Rate: 1}}
	r.advance(f)
	if r.resample != f.resampleFactor() || r.tempo != f.tempo() {
		t.Fatalf("first frame should take the filters as they are, got resample %v tempo %v", r.resample, r.tempo)
	}
}

func TestFilterRampGlidesInsteadOfJumping(t *testing.T) {
	var r filterRamp
	r.advance(Filters{})
	nightcore := Filters{Timescale: &Timescale{Speed: 1.3, Pitch: 1.3, Rate: 1}, Equalizer: []Band{{Band: 0, Gain: 0.2}}}

	r.advance(nightcore)
	if r.resample >= 1.3 || r.resample <= 1 {
		t.Fatalf("one frame in, pitch should be part-way, got %v", r.resample)
	}
	if r.eq[0] >= 0.2 || r.eq[0] <= 0 {
		t.Fatalf("one frame in, the bass band should be part-way, got %v", r.eq[0])
	}

	for i := 0; i < framesFor(1000); i++ {
		r.advance(nightcore)
	}
	if r.resample != 1.3 || r.eq[0] != 0.2 {
		t.Fatalf("after a second the ramp should have settled, got resample %v band %v", r.resample, r.eq[0])
	}
}

func TestFilterRampMostlyArrivesWithinHalfASecond(t *testing.T) {
	var r filterRamp
	r.advance(Filters{})
	target := Filters{Timescale: &Timescale{Speed: 0.8, Pitch: 0.8, Rate: 1}}
	for i := 0; i < framesFor(500); i++ {
		r.advance(target)
	}
	if math.Abs(r.resample-0.8)/0.2 > 0.1 {
		t.Fatalf("half a second in, pitch should be within 10%% of the way, got %v", r.resample)
	}
}

func TestFilterRampFadesAnEffectOutWithItsLastSettings(t *testing.T) {
	var r filterRamp
	on := Filters{Rotation: &Rotation{RotationHz: 0.2}}
	r.advance(on)
	if r.wet[wetRotation] != 1 {
		t.Fatalf("an effect present from the start is fully wet, got %v", r.wet[wetRotation])
	}

	r.advance(Filters{})
	if r.wet[wetRotation] <= 0 || r.wet[wetRotation] >= 1 {
		t.Fatalf("turning it off should fade, got %v", r.wet[wetRotation])
	}
	if eff := r.effective(); eff.Rotation == nil || eff.Rotation.RotationHz != 0.2 {
		t.Fatalf("while fading out the effect keeps its last settings, got %+v", eff.Rotation)
	}

	for i := 0; i < framesFor(1500); i++ {
		r.advance(Filters{})
	}
	if eff := r.effective(); eff.Rotation != nil {
		t.Fatalf("once faded out the effect is gone, got %+v", eff.Rotation)
	}
}
