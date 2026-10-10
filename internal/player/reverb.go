package player

import (
	"math"

	"github.com/greed/cadence/internal/audio"
)

type Reverb struct {
	Mix     float64 `json:"mix"`
	Room    float64 `json:"room"`
	Damping float64 `json:"damping"`
}

var (
	combTuning    = [8]int{1116, 1188, 1277, 1356, 1422, 1491, 1557, 1617}
	allpassTuning = [4]int{556, 441, 341, 225}
)

const (
	reverbSpread    = 23
	reverbInputGain = 0.015
	allpassFeedback = 0.5
)

type comb struct {
	buf   []float64
	idx   int
	store float64
}

func (c *comb) process(in, feedback, damp float64) float64 {
	out := c.buf[c.idx]
	c.store = out*(1-damp) + c.store*damp
	c.buf[c.idx] = in + c.store*feedback
	c.idx++
	if c.idx == len(c.buf) {
		c.idx = 0
	}
	return out
}

type allpass struct {
	buf []float64
	idx int
}

func (a *allpass) process(in float64) float64 {
	held := a.buf[a.idx]
	out := held - in
	a.buf[a.idx] = in + held*allpassFeedback
	a.idx++
	if a.idx == len(a.buf) {
		a.idx = 0
	}
	return out
}

type reverbState struct {
	ready bool
	combL [8]comb
	combR [8]comb
	apL   [4]allpass
	apR   [4]allpass
}

func scaled(samples int) int {
	return max(1, int(math.Round(float64(samples)*float64(audio.SampleRate)/44100)))
}

func (s *reverbState) init() {
	for i, n := range combTuning {
		s.combL[i].buf = make([]float64, scaled(n))
		s.combR[i].buf = make([]float64, scaled(n+reverbSpread))
	}
	for i, n := range allpassTuning {
		s.apL[i].buf = make([]float64, scaled(n))
		s.apR[i].buf = make([]float64, scaled(n+reverbSpread))
	}
	s.ready = true
}

func clamp01(v float64) float64 {
	return math.Max(0, math.Min(1, v))
}

func (s *reverbState) process(l, r float64, rv *Reverb, wet float64) (float64, float64) {
	if !s.ready {
		s.init()
	}
	feedback := clamp01(rv.Room)*0.28 + 0.7
	damp := clamp01(rv.Damping) * 0.4
	in := (l + r) * reverbInputGain
	var outL, outR float64
	for i := range s.combL {
		outL += s.combL[i].process(in, feedback, damp)
		outR += s.combR[i].process(in, feedback, damp)
	}
	for i := range s.apL {
		outL = s.apL[i].process(outL)
		outR = s.apR[i].process(outR)
	}
	amount := clamp01(rv.Mix) * wet
	return l*(1-amount*0.5) + outL*amount*3, r*(1-amount*0.5) + outR*amount*3
}
