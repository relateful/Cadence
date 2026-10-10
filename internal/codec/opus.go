package codec

import "errors"

const (
	MaxOpusBytes      = 4000
	DefaultBitrate    = 96_000
	MinBitrate        = 16_000
	MaxBitrate        = 384_000
	DefaultComplexity = 10
)

var ErrNoNativeOpus = errors.New("cadence was built without the native tag, so libopus is unavailable")

type Encoder interface {
	Encode(pcm []int16, out []byte) (int, error)
	SetBitrate(bps int) error
	Close()
}

func ClampBitrate(bps int) int {
	if bps <= 0 {
		return DefaultBitrate
	}
	return min(MaxBitrate, max(MinBitrate, bps))
}

var SilenceFrame = []byte{0xF8, 0xFF, 0xFE}
