package audio

import "math"

type HostShape struct {
	VoiceMs   int64
	OutFadeMs int64
	LeadMs    int64
	RampMs    int64
	Duck      float64
}

func DuckGain(db float64) float64 {
	return math.Pow(10, -math.Abs(db)/20)
}

func (s HostShape) TotalMs() int64 {
	return s.VoiceMs + s.RampMs
}

func (s HostShape) InOpenMs() int64 {
	return max(0, s.VoiceMs-s.LeadMs)
}

func smooth(x float64) float64 {
	x = math.Min(1, math.Max(0, x))
	return x * x * (3 - 2*x)
}

func (s HostShape) OutGain(t int64) float64 {
	if s.OutFadeMs <= 0 || t >= s.OutFadeMs {
		return 0
	}
	if t <= 0 {
		return 1
	}
	return math.Cos(math.Pi / 2 * float64(t) / float64(s.OutFadeMs))
}

func (s HostShape) InGain(t int64) float64 {
	open := s.InOpenMs()
	switch {
	case t < open:
		return 0
	case t < s.VoiceMs:
		return s.Duck * smooth(float64(t-open)/float64(max(1, s.VoiceMs-open)))
	case t < s.TotalMs():
		return s.Duck + (1-s.Duck)*smooth(float64(t-s.VoiceMs)/float64(max(1, s.RampMs)))
	}
	return 1
}

func (s HostShape) InOpen(t int64) bool {
	return t >= s.InOpenMs()
}

type Bus [FrameSamples]float64

func (b *Bus) Reset() {
	*b = Bus{}
}

func (b *Bus) Add(src *Frame, from, to float64) {
	if from == 0 && to == 0 {
		return
	}
	for i := 0; i < FrameSamplesPerChannel; i++ {
		g := from + (to-from)*float64(i+1)/FrameSamplesPerChannel
		for c := 0; c < Channels; c++ {
			k := i*Channels + c
			b[k] += float64(src[k]) * g
		}
	}
}

func (b *Bus) Write(out *Frame) {
	for i := range out {
		out[i] = clip(b[i])
	}
}
