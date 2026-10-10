package audio

import "math"

const (
	BassHz     = 220.0
	MinSweepHz = 25.0
)

type DeckDsp struct {
	bassLeft, bassRight   Biquad
	sweepLeft, sweepRight Biquad
	bassReady             bool
	sweepHz               float64
}

func NewDeckDsp() *DeckDsp {
	d := &DeckDsp{}
	d.Reset()
	return d
}

func (d *DeckDsp) Reset() {
	d.bassLeft, d.bassRight = *NewBiquad(), *NewBiquad()
	d.sweepLeft, d.sweepRight = *NewBiquad(), *NewBiquad()
	d.bassReady = false
	d.sweepHz = -1
}

func (d *DeckDsp) ApplyBassGain(f *Frame, gain float64) {
	if gain >= 0.999 {
		d.primeBass(f)
		return
	}
	if !d.bassReady {
		d.bassLeft.LowPass(BassHz, SampleRate)
		d.bassRight.LowPass(BassHz, SampleRate)
		d.bassReady = true
	}
	cut := 1 - gain
	for i := 0; i < len(f); i += 2 {
		l, r := float64(f[i]), float64(f[i+1])
		f[i] = clip(l - cut*d.bassLeft.Process(l))
		f[i+1] = clip(r - cut*d.bassRight.Process(r))
	}
}

func (d *DeckDsp) ApplyHighPass(f *Frame, cutoffHz float64) {
	if cutoffHz <= MinSweepHz {
		return
	}
	if math.Abs(cutoffHz-d.sweepHz) > 1 {
		d.sweepLeft.HighPass(cutoffHz, SampleRate)
		d.sweepRight.HighPass(cutoffHz, SampleRate)
		d.sweepHz = cutoffHz
	}
	for i := 0; i < len(f); i += 2 {
		f[i] = clip(d.sweepLeft.Process(float64(f[i])))
		f[i+1] = clip(d.sweepRight.Process(float64(f[i+1])))
	}
}

func (d *DeckDsp) primeBass(f *Frame) {
	if !d.bassReady {
		return
	}
	for i := 0; i < len(f); i += 2 {
		d.bassLeft.Process(float64(f[i]))
		d.bassRight.Process(float64(f[i+1]))
	}
}
