package player

import (
	"math"
	"testing"
)

func TestReverbLeavesATailThatDecays(t *testing.T) {
	var s reverbState
	rv := &Reverb{Mix: 0.5, Room: 0.8, Damping: 0.4}
	energy := func(from, to int, first bool) float64 {
		total := 0.0
		for i := from; i < to; i++ {
			in := 0.0
			if first && i == 0 {
				in = 1
			}
			l, r := s.process(in, in, rv, 1)
			total += l*l + r*r
		}
		return total
	}
	early := energy(0, 9600, true)
	late := energy(9600, 96000, false)
	tail := energy(96000, 192000, false)
	if early <= 0 || late <= 0 {
		t.Fatalf("an impulse should ring on, got early %v late %v", early, late)
	}
	if tail >= late {
		t.Fatalf("the tail should decay, got %v after %v", tail, late)
	}
}

func TestReverbWithNoMixIsDry(t *testing.T) {
	var s reverbState
	for i := 0; i < 2000; i++ {
		in := math.Sin(float64(i) / 9)
		l, r := s.process(in, -in, &Reverb{Mix: 0, Room: 0.9, Damping: 0.2}, 1)
		if l != in || r != -in {
			t.Fatalf("mix 0 must pass the signal through, sample %d got %v %v", i, l, r)
		}
	}
}

func TestReverbStaysFiniteAtTheLargestRoom(t *testing.T) {
	var s reverbState
	rv := &Reverb{Mix: 1, Room: 1, Damping: 0}
	for i := 0; i < 480000; i++ {
		in := math.Sin(float64(i) / 7)
		l, r := s.process(in, in, rv, 1)
		if math.IsNaN(l) || math.IsInf(l, 0) || math.Abs(l) > 50 || math.Abs(r) > 50 {
			t.Fatalf("reverb ran away at sample %d: %v %v", i, l, r)
		}
	}
}
