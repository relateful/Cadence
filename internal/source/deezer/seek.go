package deezer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/greed/cadence/internal/cache"
	"github.com/greed/cadence/internal/codec"
	"github.com/greed/cadence/internal/metrics"
	"github.com/greed/cadence/internal/source"
)

const (
	seekMinMs        = 10_000
	seekMarginFrames = 4
	layoutProbeBytes = 8 * stripeChunk
	syncSearch       = 32
	seekPrefixBytes  = 4096
	layoutEntries    = 50_000
	mpeg1Layer3SPF   = 1152
)

var (
	bitratesV1L3 = [16]int{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 0}
	ratesV1      = [4]int{44100, 48000, 32000, 0}

	errNotCBR      = errors.New("stream is not constant-bitrate mpeg-1 layer iii")
	errNoFrameSync = errors.New("no frame header at the computed offset")
)

type rangeDecryptor interface {
	DecryptFrom(trackID string, encrypted io.Reader, firstChunk int) (io.Reader, error)
}

type mp3Layout struct {
	dataStart  int64
	bitrate    int
	rate       int
	frameBytes float64
}

type frameHeader struct {
	bitrate, rate, size int
	mono, crc           bool
}

func parseFrameHeader(b []byte) (frameHeader, bool) {
	if len(b) < 4 || b[0] != 0xFF || b[1]&0xFE != 0xFA {
		return frameHeader{}, false
	}
	br, sr := bitratesV1L3[b[2]>>4], ratesV1[(b[2]>>2)&3]
	if br == 0 || sr == 0 {
		return frameHeader{}, false
	}
	pad := int(b[2]>>1) & 1
	return frameHeader{bitrate: br, rate: sr, size: 144*br*1000/sr + pad, mono: b[3]>>6 == 3, crc: b[1]&1 == 0}, true
}

func parseLayout(p []byte) (mp3Layout, error) {
	start := 0
	if len(p) >= 10 && string(p[:3]) == "ID3" {
		start = 10 + (int(p[6]&0x7F)<<21 | int(p[7]&0x7F)<<14 | int(p[8]&0x7F)<<7 | int(p[9]&0x7F))
		if p[5]&0x10 != 0 {
			start += 10
		}
	}
	if start+40 > len(p) {
		return mp3Layout{}, fmt.Errorf("%w: header beyond the probed %d bytes", errNotCBR, len(p))
	}
	h, ok := parseFrameHeader(p[start:])
	if !ok {
		return mp3Layout{}, errNotCBR
	}
	side := 32
	if h.mono {
		side = 17
	}
	if h.crc {
		side += 2
	}
	if tag := string(p[start+4+side : start+8+side]); tag == "Xing" {
		return mp3Layout{}, fmt.Errorf("%w: xing vbr header", errNotCBR)
	}
	if next, ok := parseFrameHeader(p[min(len(p), start+h.size):]); !ok || next.bitrate != h.bitrate || next.rate != h.rate {
		return mp3Layout{}, fmt.Errorf("%w: second frame does not match the first", errNotCBR)
	}
	return mp3Layout{dataStart: int64(start), bitrate: h.bitrate, rate: h.rate, frameBytes: 144 * float64(h.bitrate) * 1000 / float64(h.rate)}, nil
}

func (l mp3Layout) offset(frame int64) int64 {
	return l.dataStart + int64(math.Floor(float64(frame)*l.frameBytes))
}

func (l mp3Layout) findFrame(buf []byte, at int) (int, bool) {
	for d := 0; d <= syncSearch; d++ {
		for _, pos := range []int{at - d, at + d} {
			if pos < 0 || pos+4 > len(buf) {
				continue
			}
			h, ok := parseFrameHeader(buf[pos:])
			if !ok || h.bitrate != l.bitrate || h.rate != l.rate {
				continue
			}
			if next, ok := parseFrameHeader(buf[min(len(buf), pos+h.size):]); ok && next.bitrate == l.bitrate && next.rate == l.rate {
				return pos, true
			}
		}
	}
	return 0, false
}

type seekSource struct {
	format string
	open   func(offset int64) (io.ReadCloser, error)
	done   func()
}

func (d *Deezer) diskSeekSource(trackID string) (seekSource, bool) {
	if d.disk == nil {
		return seekSource{}, false
	}
	for _, f := range d.formats {
		key := cacheKey(trackID, f.Format)
		if !d.disk.store.Has(key) {
			continue
		}
		file, size, ok := d.disk.store.Get(key)
		if !ok {
			continue
		}
		return seekSource{
			format: f.Format,
			open: func(off int64) (io.ReadCloser, error) {
				return io.NopCloser(io.NewSectionReader(file, off, size-off)), nil
			},
			done: func() { file.Close() },
		}, true
	}
	return seekSource{}, false
}

func (d *Deezer) cdnSeekSource(ctx context.Context, trackID string) (seekSource, error) {
	link, err := d.mediaURL(ctx, trackID)
	if err != nil {
		return seekSource{}, err
	}
	return seekSource{
		format: link.Format,
		open: func(off int64) (io.ReadCloser, error) {
			body, _, err := d.rangeFrom(ctx, link.URL, off)
			return body, err
		},
		done: func() {},
	}, nil
}

func (d *Deezer) probeLayout(trackID string, src seekSource, dec rangeDecryptor) (mp3Layout, error) {
	key := cacheKey(trackID, src.format)
	if l, ok := d.layouts.Get(key); ok {
		return l, nil
	}
	body, err := src.open(0)
	if err != nil {
		return mp3Layout{}, err
	}
	defer body.Close()
	plain, err := dec.DecryptFrom(trackID, io.LimitReader(body, layoutProbeBytes), 0)
	if err != nil {
		return mp3Layout{}, err
	}
	probe, err := io.ReadAll(plain)
	if err != nil {
		return mp3Layout{}, err
	}
	l, err := parseLayout(probe)
	if err != nil {
		return mp3Layout{}, err
	}
	d.layouts.Set(key, l)
	return l, nil
}

func (d *Deezer) OpenAt(ctx context.Context, t source.Track, startMs int64) (source.Stream, error) {
	if startMs >= seekMinMs {
		stream, err := d.seekOpen(ctx, t.Identifier, startMs)
		if err == nil {
			return stream, nil
		}
		metrics.Seek.Inc("fallback")
		d.log.Debug("range seek unavailable, streaming from the start", "track", t.Identifier, "error", err)
	}
	return d.Open(ctx, t)
}

func (d *Deezer) seekOpen(ctx context.Context, trackID string, startMs int64) (source.Stream, error) {
	dec, ok := registered().(rangeDecryptor)
	if !ok {
		return source.Stream{}, errors.New("decryptor cannot start mid-file")
	}
	src, onDisk := d.diskSeekSource(trackID)
	if !onDisk {
		var err error
		if src, err = d.cdnSeekSource(ctx, trackID); err != nil {
			return source.Stream{}, err
		}
	}
	ok = false
	defer func() {
		if !ok {
			src.done()
		}
	}()
	layout, err := d.probeLayout(trackID, src, dec)
	if err != nil {
		return source.Stream{}, err
	}
	target := startMs * int64(layout.rate) / 1000 / mpeg1Layer3SPF
	first := target - seekMarginFrames
	if first < 1 {
		return source.Stream{}, errors.New("target is inside the first frames")
	}
	est := layout.offset(first)
	chunkStart := max(0, est-syncSearch) / stripeChunk * stripeChunk
	body, err := src.open(chunkStart)
	if err != nil {
		return source.Stream{}, err
	}
	plain, err := dec.DecryptFrom(trackID, body, int(chunkStart/stripeChunk))
	if err != nil {
		body.Close()
		return source.Stream{}, err
	}
	prefix := make([]byte, int(est-chunkStart)+seekPrefixBytes)
	n, err := io.ReadFull(plain, prefix)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		body.Close()
		return source.Stream{}, err
	}
	pos, found := layout.findFrame(prefix[:n], int(est-chunkStart))
	if !found {
		body.Close()
		return source.Stream{}, errNoFrameSync
	}
	ok = true
	if onDisk {
		metrics.Seek.Inc("disk")
	} else {
		metrics.Seek.Inc("range")
	}
	closer := closeBoth{body, src.done}
	return source.Stream{
		Body:        decryptedBody{Reader: io.MultiReader(bytes.NewReader(prefix[pos:n]), plain), closer: closer},
		Format:      codec.FormatMP3,
		StartSample: first * mpeg1Layer3SPF,
	}, nil
}

type closeBoth struct {
	c    io.Closer
	done func()
}

func (c closeBoth) Close() error {
	err := c.c.Close()
	c.done()
	return err
}

func newLayoutCache() *cache.LRU[string, mp3Layout] {
	return cache.New[string, mp3Layout](layoutEntries, 0)
}
