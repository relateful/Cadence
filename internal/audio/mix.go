package audio

func Mix(a *Frame, aGain float64, b *Frame, bGain float64, out *Frame) {
	for i := range out {
		out[i] = clip(float64(a[i])*aGain + float64(b[i])*bGain)
	}
}

func Scale(src *Frame, gain float64, out *Frame) {
	if gain == 1 {
		*out = *src
		return
	}
	for i := range out {
		out[i] = clip(float64(src[i]) * gain)
	}
}
