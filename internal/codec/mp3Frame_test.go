package codec

import (
	"encoding/binary"
	"testing"
)

func frames(n int, xing string, count uint32) []byte {
	const size = 417
	var out []byte
	for i := range n {
		f := make([]byte, size)
		copy(f, []byte{0xFF, 0xFB, 0x90, 0x64})
		if i == 0 && xing != "" {
			copy(f[36:], xing)
			binary.BigEndian.PutUint32(f[40:], 1)
			binary.BigEndian.PutUint32(f[44:], count)
		}
		out = append(out, f...)
	}
	return out
}

func TestParseMP3FrameReadsMPEG1Layer3(t *testing.T) {
	f, ok := ParseMP3Frame([]byte{0xFF, 0xFB, 0x90, 0x64})
	if !ok || f.Bitrate != 128 || f.Rate != 44100 || f.Size != 417 || f.Mono || f.CRC {
		t.Fatalf("%+v %v", f, ok)
	}
	if f.TagOffset() != 36 {
		t.Fatalf("tag offset %d", f.TagOffset())
	}
	if _, ok := ParseMP3Frame([]byte{0xFF, 0xF3, 0x90, 0x64}); ok {
		t.Fatal("accepted an mpeg-2 header")
	}
}

func TestProbeMP3EstimatesCBRFromSize(t *testing.T) {
	head := frames(20, "", 0)
	info, ok := ProbeMP3(head, 128_000/8*180)
	if !ok || info.DurationMs != 180_000 || info.VBR {
		t.Fatalf("%+v %v", info, ok)
	}
}

func TestProbeMP3TrustsTheXingFrameCount(t *testing.T) {
	head := frames(20, "Xing", 1000)
	info, ok := ProbeMP3(head, 1)
	if !ok || !info.VBR || info.DurationMs != 1000*1152*1000/44100 {
		t.Fatalf("%+v %v", info, ok)
	}
}

func TestProbeMP3SkipsJunkAndRejectsNonAudio(t *testing.T) {
	head := append([]byte("garbage before the first frame"), frames(5, "", 0)...)
	if _, ok := ProbeMP3(head, int64(len(head))); !ok {
		t.Fatal("did not find the first frame after junk")
	}
	if _, ok := ProbeMP3([]byte("<html><body>not audio</body></html>"), 40); ok {
		t.Fatal("html probed as mp3")
	}
}

func TestID3SizeIncludesTheFooter(t *testing.T) {
	tag := []byte{'I', 'D', '3', 4, 0, 0x10, 0, 0, 1, 0}
	if got := ID3Size(tag); got != 10+128+10 {
		t.Fatalf("%d", got)
	}
	if ID3Size([]byte{0xFF, 0xFB, 0x90, 0x64}) != 0 {
		t.Fatal("a frame header read as a tag")
	}
}
