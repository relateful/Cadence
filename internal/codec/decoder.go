package codec

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/hajimehoshi/go-mp3"
)

type Format int

const (
	FormatMP3 Format = iota
	FormatPCM48
)

type pcmStream struct {
	r   io.Reader
	raw []byte
}

func (s *pcmStream) SampleRate() int {
	return 48_000
}

func (s *pcmStream) Read(samples []int16) (int, error) {
	need := len(samples) * 2
	if cap(s.raw) < need {
		s.raw = make([]byte, need)
	}
	n, err := io.ReadFull(s.r, s.raw[:need])
	count := n / 2
	for i := 0; i < count; i++ {
		samples[i] = int16(binary.LittleEndian.Uint16(s.raw[2*i:]))
	}
	if err == io.ErrUnexpectedEOF {
		err = io.EOF
	}
	return count, err
}

type PCMStream interface {
	Read(samples []int16) (int, error)
	SampleRate() int
}

type mp3Stream struct {
	dec  *mp3.Decoder
	raw  []byte
	rate int
}

func Decode(format Format, r io.Reader) (PCMStream, error) {
	switch format {
	case FormatMP3:
		dec, err := mp3.NewDecoder(bufio.NewReaderSize(r, 64<<10))
		if err != nil {
			return nil, fmt.Errorf("mp3: %w", err)
		}
		return &mp3Stream{dec: dec, rate: dec.SampleRate()}, nil
	case FormatPCM48:
		return &pcmStream{r: bufio.NewReaderSize(r, 64<<10)}, nil
	}
	return nil, fmt.Errorf("unsupported format %d", format)
}

func (s *mp3Stream) SampleRate() int {
	return s.rate
}

func (s *mp3Stream) Read(samples []int16) (int, error) {
	need := len(samples) * 2
	if cap(s.raw) < need {
		s.raw = make([]byte, need)
	}
	buf := s.raw[:need]
	n, err := io.ReadFull(s.dec, buf)
	count := n / 2
	for i := 0; i < count; i++ {
		samples[i] = int16(binary.LittleEndian.Uint16(buf[2*i:]))
	}
	if err == io.ErrUnexpectedEOF {
		err = io.EOF
	}
	return count, err
}
