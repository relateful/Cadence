package audio

const (
	SampleRate             = 48_000
	Channels               = 2
	FrameMs                = 20
	FrameSamplesPerChannel = SampleRate / 1000 * FrameMs
	FrameSamples           = FrameSamplesPerChannel * Channels
	FramesPerSecond        = 1000 / FrameMs
)

type Frame [FrameSamples]int16

var Silence Frame

func FramesFor(ms int64) int64 {
	return ms / FrameMs
}

func MsFor(frames int64) int64 {
	return frames * FrameMs
}

func clip(v float64) int16 {
	if v > 32767 {
		return 32767
	}
	if v < -32768 {
		return -32768
	}
	if v < 0 {
		return int16(v - 0.5)
	}
	return int16(v + 0.5)
}

func Clip(v float64) int16 {
	return clip(v)
}

func (f *Frame) Peak() int {
	peak := 0
	for _, s := range f {
		a := int(s)
		if a < 0 {
			a = -a
		}
		if a > peak {
			peak = a
		}
	}
	return peak
}
