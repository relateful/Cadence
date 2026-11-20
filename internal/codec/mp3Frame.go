package codec

import (
	"encoding/binary"
)

const MP3SamplesPerFrame = 1152

var (
	mp3Bitrates = [16]int{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 0}
	mp3Rates    = [4]int{44100, 48000, 32000, 0}
)

type MP3Frame struct {
	Bitrate int
	Rate    int
	Size    int
	Mono    bool
	CRC     bool
}

func ParseMP3Frame(b []byte) (MP3Frame, bool) {
	if len(b) < 4 || b[0] != 0xFF || b[1]&0xFE != 0xFA {
		return MP3Frame{}, false
	}
	br, sr := mp3Bitrates[b[2]>>4], mp3Rates[(b[2]>>2)&3]
	if br == 0 || sr == 0 {
		return MP3Frame{}, false
	}
	pad := int(b[2]>>1) & 1
	return MP3Frame{Bitrate: br, Rate: sr, Size: 144*br*1000/sr + pad, Mono: b[3]>>6 == 3, CRC: b[1]&1 == 0}, true
}

func (f MP3Frame) TagOffset() int {
	side := 32
	if f.Mono {
		side = 17
	}
	if f.CRC {
		side += 2
	}
	return 4 + side
}

func (f MP3Frame) FrameBytes() float64 {
	return 144 * float64(f.Bitrate) * 1000 / float64(f.Rate)
}

func ID3Size(p []byte) int {
	if len(p) < 10 || string(p[:3]) != "ID3" {
		return 0
	}
	size := 10 + (int(p[6]&0x7F)<<21 | int(p[7]&0x7F)<<14 | int(p[8]&0x7F)<<7 | int(p[9]&0x7F))
	if p[5]&0x10 != 0 {
		size += 10
	}
	return size
}

type MP3Info struct {
	DurationMs int64
	Rate       int
	VBR        bool
}

func ProbeMP3(head []byte, audioBytes int64) (MP3Info, bool) {
	for at := 0; at+4 <= len(head); at++ {
		f, ok := ParseMP3Frame(head[at:])
		if !ok {
			continue
		}
		if next := at + f.Size; next+4 <= len(head) {
			if n, ok := ParseMP3Frame(head[next:]); !ok || n.Rate != f.Rate {
				continue
			}
		}
		info := MP3Info{Rate: f.Rate}
		if tag := at + f.TagOffset(); tag+12 <= len(head) {
			switch string(head[tag : tag+4]) {
			case "Xing", "Info":
				flags := binary.BigEndian.Uint32(head[tag+4:])
				if flags&1 != 0 {
					frames := int64(binary.BigEndian.Uint32(head[tag+8:]))
					info.VBR = string(head[tag:tag+4]) == "Xing"
					info.DurationMs = frames * MP3SamplesPerFrame * 1000 / int64(f.Rate)
					return info, true
				}
			}
		}
		if audioBytes > int64(at) {
			info.DurationMs = (audioBytes - int64(at)) * 8 / int64(f.Bitrate)
		}
		return info, true
	}
	return MP3Info{}, false
}
