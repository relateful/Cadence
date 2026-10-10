package deezer

import (
	"bytes"
	"context"
	"io"
	"math"
	"strings"
	"testing"

	"github.com/greed/cadence/internal/codec"
	"github.com/greed/cadence/internal/source"
)

const synthBitrate, synthRate = 128, 44100

func synthMP3(frames int, vbr bool) []byte {
	var b bytes.Buffer
	b.Write([]byte{'I', 'D', '3', 3, 0, 0, 0, 0, 0, 100})
	b.Write(make([]byte, 100))
	exact := 144.0 * synthBitrate * 1000 / synthRate
	for i := 0; i < frames; i++ {
		size := int(math.Floor(float64(i+1)*exact) - math.Floor(float64(i)*exact))
		pad := byte(0)
		if size > 417 {
			pad = 1
		}
		f := make([]byte, size)
		f[0], f[1], f[2], f[3] = 0xFF, 0xFB, 0x90|pad<<1, 0x00
		if i == 0 {
			tag := "Info"
			if vbr {
				tag = "Xing"
			}
			copy(f[36:], tag)
		}
		b.Write(f)
	}
	return b.Bytes()
}

func decodedSamples(t *testing.T, r io.Reader) int64 {
	t.Helper()
	pcm, err := codec.Decode(codec.FormatMP3, r)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]int16, 8192)
	var total int64
	for {
		n, err := pcm.Read(buf)
		total += int64(n / 2)
		if err != nil {
			return total
		}
	}
}

func seekDeezer(t *testing.T, plain []byte) (*Deezer, source.Track, *cdn) {
	t.Helper()
	server := &cdn{}
	d, track := newTestDeezer(t, server, plain)
	return d, track, server
}

func TestLayoutParsesCBRAndRejectsVBR(t *testing.T) {
	l, err := parseLayout(synthMP3(10, false))
	if err != nil || l.dataStart != 110 || l.bitrate != 128 || l.rate != 44100 {
		t.Fatalf("%+v %v", l, err)
	}
	if _, err := parseLayout(synthMP3(10, true)); err == nil {
		t.Fatal("vbr file accepted")
	}
}

func TestRangeSeekLandsOnTheExactFrame(t *testing.T) {
	const frames = 3000
	plain := synthMP3(frames, false)
	d, track, server := seekDeezer(t, plain)
	full := decodedSamples(t, bytes.NewReader(plain))
	for _, ms := range []int64{15_000, 42_345, 60_000} {
		stream, err := d.seekOpen(context.Background(), track.Identifier, ms)
		if err != nil {
			t.Fatalf("%dms: %v", ms, err)
		}
		want := (ms*synthRate/1000/mpeg1Layer3SPF - seekMarginFrames) * mpeg1Layer3SPF
		if stream.StartSample != want {
			t.Fatalf("%dms: start sample %d, want %d", ms, stream.StartSample, want)
		}
		rest := decodedSamples(t, stream.Body)
		stream.Body.Close()
		if stream.StartSample+rest != full {
			t.Fatalf("%dms: start %d + decoded %d != full decode %d", ms, stream.StartSample, rest, full)
		}
	}
	for _, r := range server.seen() {
		if r == "" {
			t.Fatal("a seek downloaded the whole file")
		}
	}
	last := server.seen()[len(server.seen())-1]
	if strings.HasPrefix(last, "bytes=0-") {
		t.Fatalf("seek fetched from the start: %s", last)
	}
}

func TestDiskCachedTrackSeeksWithoutNetwork(t *testing.T) {
	plain := synthMP3(2000, false)
	d, track, server := seekDeezer(t, plain)
	withDisk(t, d)
	readAll(t, d, track)
	readAll(t, d, track)
	before := len(server.seen())
	stream, err := d.OpenAt(context.Background(), track, 20_000)
	if err != nil {
		t.Fatal(err)
	}
	full := decodedSamples(t, bytes.NewReader(plain))
	if got := stream.StartSample + decodedSamples(t, stream.Body); got != full {
		t.Fatalf("disk seek decoded to %d, want %d", got, full)
	}
	stream.Body.Close()
	if len(server.seen()) != before {
		t.Fatalf("disk seek made %d requests", len(server.seen())-before)
	}
}

func TestVBRTrackFallsBackToAFullStream(t *testing.T) {
	plain := synthMP3(1500, true)
	d, track, _ := seekDeezer(t, plain)
	stream, err := d.OpenAt(context.Background(), track, 20_000)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	if stream.StartSample != 0 {
		t.Fatalf("vbr stream claimed start sample %d", stream.StartSample)
	}
	got, _ := io.ReadAll(stream.Body)
	if !bytes.Equal(got, plain) {
		t.Fatal("fallback did not return the whole track")
	}
}
