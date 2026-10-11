package player

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/greed/cadence/internal/audio"
	"github.com/greed/cadence/internal/codec"
	"github.com/greed/cadence/internal/metrics"
)

const (
	MaxVoiceBytes    = 512 << 10
	MaxVoiceMs       = 25_000
	defaultOutFadeMs = 2500
	defaultLeadMs    = 1800
	defaultRampMs    = 1200
	defaultDuckDb    = 9
	maxHostFadeMs    = 8000
	voiceTargetRms   = 0.158
	voicePeakCeiling = 0.891
)

var (
	ErrVoiceTooLarge = fmt.Errorf("voice clip is larger than %d bytes", MaxVoiceBytes)
	ErrVoiceTooLong  = fmt.Errorf("voice clip is longer than %dms", MaxVoiceMs)
	ErrVoiceEmpty    = errors.New("voice clip has no audio")
	ErrTrackLoading  = errors.New("a track is still loading on deck a")
)

type HostRequest struct {
	Audio        string  `json:"audio"`
	AtPositionMs *int64  `json:"atPositionMs,omitempty"`
	OutFadeMs    int64   `json:"outFadeMs"`
	LeadMs       int64   `json:"leadMs"`
	RampMs       int64   `json:"rampMs"`
	DuckDb       float64 `json:"duckDb"`
}

type voiceClip struct {
	frames []audio.Frame
}

func (c *voiceClip) durationMs() int64 {
	return audio.MsFor(int64(len(c.frames)))
}

type hostRun struct {
	clip       *voiceClip
	shape      audio.HostShape
	elapsed    int64
	voiceEnded bool
}

func orDefault(v, fallback, ceiling int64) int64 {
	if v <= 0 {
		return fallback
	}
	return min(v, ceiling)
}

func (r HostRequest) shape(voiceMs int64) audio.HostShape {
	duck := r.DuckDb
	if duck == 0 {
		duck = defaultDuckDb
	}
	return audio.HostShape{
		VoiceMs:   voiceMs,
		OutFadeMs: orDefault(r.OutFadeMs, defaultOutFadeMs, maxHostFadeMs),
		LeadMs:    orDefault(r.LeadMs, defaultLeadMs, maxHostFadeMs),
		RampMs:    orDefault(r.RampMs, defaultRampMs, maxHostFadeMs),
		Duck:      audio.DuckGain(math.Min(30, math.Abs(duck))),
	}
}

func decodeVoice(encoded string) (*voiceClip, error) {
	if base64.StdEncoding.DecodedLen(len(encoded)) > MaxVoiceBytes+3 {
		return nil, ErrVoiceTooLarge
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("voice clip is not base64: %w", err)
	}
	if len(raw) > MaxVoiceBytes {
		return nil, ErrVoiceTooLarge
	}
	pcm, err := codec.Decode(codec.FormatMP3, bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("voice clip is not mp3: %w", err)
	}
	rate := pcm.SampleRate()
	limit := (MaxVoiceMs/1000 + 1) * rate * audio.Channels
	var samples []int16
	buf := make([]int16, 8192)
	for {
		n, rerr := pcm.Read(buf)
		samples = append(samples, buf[:n]...)
		if len(samples) > limit {
			return nil, ErrVoiceTooLong
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			return nil, fmt.Errorf("voice clip decode: %w", rerr)
		}
	}
	frames := toFrames(resampled(samples, rate))
	if len(frames) == 0 {
		return nil, ErrVoiceEmpty
	}
	clip := &voiceClip{frames: frames}
	if clip.durationMs() > MaxVoiceMs {
		return nil, ErrVoiceTooLong
	}
	level(clip.frames)
	return clip, nil
}

func resampled(samples []int16, rate int) []float64 {
	if rate == audio.SampleRate {
		out := make([]float64, len(samples))
		for i, s := range samples {
			out[i] = float64(s)
		}
		return out
	}
	r := audio.NewResampler(rate, 1)
	r.Write(samples)
	r.Write(make([]int16, 64*audio.Channels))
	out := make([]float64, 2*(r.Available()+1))
	n := r.Read(out)
	return out[:2*n]
}

func toFrames(interleaved []float64) []audio.Frame {
	count := (len(interleaved) + audio.FrameSamples - 1) / audio.FrameSamples
	frames := make([]audio.Frame, count)
	for i, v := range interleaved {
		frames[i/audio.FrameSamples][i%audio.FrameSamples] = audio.Clip(v)
	}
	return frames
}

func level(frames []audio.Frame) {
	var sum float64
	peak := 0
	n := 0
	for i := range frames {
		for _, s := range frames[i] {
			v := float64(s) / 32768
			sum += v * v
			n++
		}
		peak = max(peak, frames[i].Peak())
	}
	if n == 0 || sum == 0 || peak == 0 {
		return
	}
	gain := voiceTargetRms / math.Sqrt(sum/float64(n))
	gain = math.Min(gain, voicePeakCeiling*32768/float64(peak))
	gain = math.Min(4, math.Max(0.25, gain))
	for i := range frames {
		audio.Scale(&frames[i], gain, &frames[i])
	}
}

func (p *Player) StartHost(req HostRequest) error {
	clip, err := decodeVoice(req.Audio)
	if err != nil {
		metrics.Host.Inc("refused")
		return err
	}
	return p.armHost(req, clip)
}

func (p *Player) armHost(req HostRequest, clip *voiceClip) error {
	shape := req.shape(clip.durationMs())
	p.mu.Lock()
	if p.cued == nil {
		p.mu.Unlock()
		metrics.Host.Inc("refused")
		return ErrNoDeckB
	}
	if p.active == nil && p.loading != nil {
		p.mu.Unlock()
		metrics.Host.Inc("refused")
		return ErrTrackLoading
	}
	deck, inst := p.cued, p.cuedInst
	p.cued = nil
	run := &hostRun{clip: clip, shape: shape}
	total := int(shape.TotalMs())
	if p.active == nil {
		run.shape.OutFadeMs = 0
		p.hostPosA = 0
		p.paused, p.env, p.fadingOut = false, 1, false
	}
	if req.AtPositionMs != nil && p.active != nil {
		if p.pending != nil {
			p.dropDeckLocked(p.pending.deck, p.pending.instance)
		}
		p.pending = &pendingMix{deck: deck, instance: inst, durationMs: total, curve: audio.CurveLinear, atMs: *req.AtPositionMs, gain: 1, host: run}
		p.mu.Unlock()
		p.emit(Event{Type: EventMixArmed, Track: &deck.Track, Instance: inst, PositionMs: *req.AtPositionMs, DurationMs: int64(total)})
		return nil
	}
	p.beginFadeLocked(deck, inst, total, audio.CurveLinear, audio.TransitionProfile{}, 1, run)
	p.mu.Unlock()
	return nil
}

func (p *Player) hostFrameLocked(out *audio.Frame, vol float64) (FrameInfo, bool) {
	h := p.host
	t0, t1 := h.elapsed, h.elapsed+audio.FrameMs
	p.bus.Reset()
	posA := p.hostPosA
	if from, to := h.shape.OutGain(t0), h.shape.OutGain(t1); p.active != nil && (from > 0 || to > 0) {
		if pos, ok := p.active.Read(&p.bufA); ok {
			posA = pos
			g := p.activeGain * p.active.norm * vol
			p.bus.Add(&p.bufA, from*g, to*g)
		}
	}
	if t1 > h.shape.InOpenMs() {
		if _, ok := p.incoming.Read(&p.bufB); ok {
			g := p.incGain * p.incoming.norm * vol
			p.bus.Add(&p.bufB, h.shape.InGain(t0)*g, h.shape.InGain(t1)*g)
		}
	}
	if idx := t0 / audio.FrameMs; idx < int64(len(h.clip.frames)) {
		p.bus.Add(&h.clip.frames[idx], vol, vol)
	}
	p.bus.Write(out)
	p.hostPosA = posA
	h.elapsed = t1
	p.fade.Advance()
	info := FrameInfo{Instance: p.activeInst, PositionMs: posA, Mixing: true}
	if !h.voiceEnded && t1 >= h.shape.VoiceMs {
		h.voiceEnded = true
		p.emit(Event{Type: EventHostEnded, Track: p.activeTrackLocked(), Instance: p.activeInst, DurationMs: h.shape.VoiceMs})
	}
	if t1 >= h.shape.TotalMs() {
		metrics.Host.Inc("played")
		p.promoteLocked()
	}
	return info, true
}
