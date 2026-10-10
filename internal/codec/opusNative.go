//go:build native

package codec

import (
	"github.com/greed/cadence/internal/audio"
	"gopkg.in/hraban/opus.v2"
)

type nativeEncoder struct {
	enc *opus.Encoder
}

func NewEncoder(bitrate int) (Encoder, error) {
	enc, err := opus.NewEncoder(audio.SampleRate, audio.Channels, opus.AppAudio)
	if err != nil {
		return nil, err
	}
	if err := enc.SetComplexity(DefaultComplexity); err != nil {
		return nil, err
	}
	if err := enc.SetBitrate(ClampBitrate(bitrate)); err != nil {
		return nil, err
	}
	return &nativeEncoder{enc: enc}, nil
}

func (e *nativeEncoder) Encode(pcm []int16, out []byte) (int, error) {
	return e.enc.Encode(pcm, out)
}

func (e *nativeEncoder) SetBitrate(bps int) error {
	return e.enc.SetBitrate(ClampBitrate(bps))
}

func (e *nativeEncoder) Close() {}
