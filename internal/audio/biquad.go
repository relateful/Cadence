package audio

import "math"

const butterworthQ = 0.7071

type Biquad struct {
	b0, b1, b2, a1, a2 float64
	x1, x2, y1, y2     float64
}

func NewBiquad() *Biquad {
	return &Biquad{b0: 1}
}

func clampCutoff(hz float64, sampleRate int) float64 {
	return math.Max(20, math.Min(hz, float64(sampleRate)/2-100))
}

func (b *Biquad) LowPass(cutoffHz float64, sampleRate int) {
	w0 := 2 * math.Pi * clampCutoff(cutoffHz, sampleRate) / float64(sampleRate)
	cos := math.Cos(w0)
	alpha := math.Sin(w0) / (2 * butterworthQ)
	a0 := 1 + alpha
	b.b0 = ((1 - cos) / 2) / a0
	b.b1 = (1 - cos) / a0
	b.b2 = b.b0
	b.a1 = (-2 * cos) / a0
	b.a2 = (1 - alpha) / a0
}

func (b *Biquad) HighPass(cutoffHz float64, sampleRate int) {
	w0 := 2 * math.Pi * clampCutoff(cutoffHz, sampleRate) / float64(sampleRate)
	cos := math.Cos(w0)
	alpha := math.Sin(w0) / (2 * butterworthQ)
	a0 := 1 + alpha
	b.b0 = ((1 + cos) / 2) / a0
	b.b1 = -(1 + cos) / a0
	b.b2 = b.b0
	b.a1 = (-2 * cos) / a0
	b.a2 = (1 - alpha) / a0
}

func (b *Biquad) Coefficients(b0, b1, b2, a1, a2 float64) {
	b.b0, b.b1, b.b2, b.a1, b.a2 = b0, b1, b2, a1, a2
}

func (b *Biquad) Process(x float64) float64 {
	y := b.b0*x + b.b1*b.x1 + b.b2*b.x2 - b.a1*b.y1 - b.a2*b.y2
	b.x2, b.x1 = b.x1, x
	b.y2, b.y1 = b.y1, y
	return y
}

func (b *Biquad) Reset() {
	b.x1, b.x2, b.y1, b.y2 = 0, 0, 0, 0
}
