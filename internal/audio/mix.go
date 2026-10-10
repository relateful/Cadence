package audio

func Mix(a *Frame, aGain float64, b *Frame, bGain float64, out *Frame) {
	for i := range out {
		out[i] = clip(float64(a[i])*aGain + float64(b[i])*bGain)
	}
}

func Ramp(f *Frame, from, to float64) {
	for i := 0; i < FrameSamplesPerChannel; i++ {
		g := from + (to-from)*float64(i+1)/FrameSamplesPerChannel
		for c := 0; c < Channels; c++ {
			k := i*Channels + c
			f[k] = clip(float64(f[k]) * g)
		}
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
