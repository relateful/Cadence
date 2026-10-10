package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"time"

	"github.com/greed/cadence/internal/codec"
	"github.com/greed/cadence/internal/config"
	"github.com/greed/cadence/internal/httpx"
	"github.com/greed/cadence/internal/source/deezer"
)

func main() {
	if err := run(); err != nil {
		slog.Error("livecheck failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.FromEnv()
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	dec, err := deezer.NewStripeDecryptor(cfg.DeezerMasterKey)
	if err != nil {
		return err
	}
	deezer.RegisterDecryptor(dec)
	client, err := httpx.New(deezer.Name, cfg.DeezerProxy, log)
	if err != nil {
		return err
	}
	dz := deezer.New(client, cfg.DeezerARL, cfg.DeezerHighQuality, log)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if len(os.Args) > 1 && os.Args[1] == "-seek" {
		return seekCheck(ctx, dz)
	}
	if len(os.Args) > 1 {
		return bench(ctx, dz, os.Args[1:])
	}

	tracks, err := dz.Search(ctx, "daft punk one more time")
	if err != nil {
		return fmt.Errorf("search: %w", err)
	}
	if len(tracks) == 0 {
		return errors.New("search returned nothing")
	}
	t := tracks[0]
	fmt.Printf("track:    %s - %s (deezer %s, %ds)\n", t.Author, t.Title, t.Identifier, t.DurationMs/1000)

	start := time.Now()
	stream, err := dz.Open(ctx, t)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer stream.Body.Close()
	pcm, err := codec.Decode(stream.Format, stream.Body)
	if err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	buf := make([]int16, 4096)
	var total int
	var sum, peak, jump float64
	var prev int16
	for {
		n, err := pcm.Read(buf)
		for _, s := range buf[:n] {
			v := float64(s)
			sum += v * v
			peak = max(peak, math.Abs(v))
			jump += math.Abs(v - float64(prev))
			prev = s
		}
		total += n
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read after %d samples: %w", total, err)
		}
	}
	if total == 0 {
		return errors.New("decoded no audio")
	}
	rms := math.Sqrt(sum/float64(total)) / 32768
	smooth := jump / float64(total) / 32768
	fmt.Printf("decoded:  %.1fs of audio at %d Hz in %s\n", float64(total)/2/float64(pcm.SampleRate()), pcm.SampleRate(), time.Since(start).Round(time.Millisecond))
	fmt.Printf("level:    rms %.3f, peak %.3f, mean step %.4f\n", rms, peak/32768, smooth)
	if rms < 0.01 {
		return errors.New("audio is near silent")
	}
	if smooth > 0.25 {
		return errors.New("audio looks like noise; the master key is probably wrong")
	}
	fmt.Println("result:   real music decoded; the Deezer stream path works")
	return nil
}

func bench(ctx context.Context, dz *deezer.Deezer, queries []string) error {
	fmt.Println("query\tsearch_ms\topen_ms\tfirst_audio_ms\ttotal_ms")
	buf := make([]int16, 1920)
	for _, q := range queries {
		t0 := time.Now()
		tracks, err := dz.Search(ctx, q)
		if err != nil {
			return fmt.Errorf("search %q: %w", q, err)
		}
		if len(tracks) == 0 {
			return fmt.Errorf("search %q returned nothing", q)
		}
		t1 := time.Now()
		stream, err := dz.Open(ctx, tracks[0])
		if err != nil {
			return fmt.Errorf("open %q: %w", q, err)
		}
		t2 := time.Now()
		pcm, err := codec.Decode(stream.Format, stream.Body)
		if err == nil {
			_, err = pcm.Read(buf)
		}
		t3 := time.Now()
		stream.Body.Close()
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("decode %q: %w", q, err)
		}
		fmt.Printf("%s\t%d\t%d\t%d\t%d\n", q, t1.Sub(t0).Milliseconds(), t2.Sub(t1).Milliseconds(), t3.Sub(t2).Milliseconds(), t3.Sub(t0).Milliseconds())
	}
	return nil
}

func decodeAll(r io.Reader) ([]int16, error) {
	pcm, err := codec.Decode(codec.FormatMP3, r)
	if err != nil {
		return nil, err
	}
	var out []int16
	buf := make([]int16, 8192)
	for {
		n, err := pcm.Read(buf)
		out = append(out, buf[:n]...)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
	}
}

func seekCheck(ctx context.Context, dz *deezer.Deezer) error {
	tracks, err := dz.Search(ctx, "daft punk one more time")
	if err != nil || len(tracks) == 0 {
		return fmt.Errorf("search: %v", err)
	}
	t := tracks[0]
	stream, err := dz.Open(ctx, t)
	if err != nil {
		return err
	}
	full, err := decodeAll(stream.Body)
	stream.Body.Close()
	if err != nil {
		return fmt.Errorf("full decode: %w", err)
	}
	fmt.Printf("track:    %s - %s, %d samples decoded in full\n", t.Author, t.Title, len(full)/2)
	for _, ms := range []int64{15_000, 61_234, 180_000} {
		start := time.Now()
		s, err := dz.OpenAt(ctx, t, ms)
		if err != nil {
			return err
		}
		pcm, err := codec.Decode(s.Format, s.Body)
		if err != nil {
			s.Body.Close()
			return err
		}
		want := 2 * 44100 * 5
		got := make([]int16, 0, want)
		buf := make([]int16, 4096)
		for len(got) < want {
			n, err := pcm.Read(buf)
			got = append(got, buf[:n]...)
			if err != nil {
				break
			}
		}
		took := time.Since(start)
		s.Body.Close()
		if s.StartSample == 0 {
			fmt.Printf("seek %6dms: fell back to a full stream (%s)\n", ms, took.Round(time.Millisecond))
			continue
		}
		off := int(s.StartSample) * 2
		skip := 2 * 1152 * 4
		for i := skip; i < len(got); i++ {
			if off+i >= len(full) || got[i] != full[off+i] {
				return fmt.Errorf("seek %dms: sample %d differs from the full decode", ms, i/2)
			}
		}
		fmt.Printf("seek %6dms: start sample %d, %d samples bit-exact after the warm-up frames, opened in %s\n", ms, s.StartSample, (len(got)-skip)/2, took.Round(time.Millisecond))
	}
	fmt.Println("result:   range seeks match the full decode")
	return nil
}
