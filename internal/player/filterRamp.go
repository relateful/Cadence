package player

import (
	"math"

	"github.com/greed/cadence/internal/audio"
)

const (
	FilterRampTauMs = 150.0
	rampSnap        = 1e-3
)

const (
	wetKaraoke = iota
	wetLowPass
	wetVibrato
	wetTremolo
	wetRotation
	wetReverb
	wetCount
)

var rampAlpha = 1 - math.Exp(-float64(audio.FrameMs)/FilterRampTauMs)

type filterRamp struct {
	ready    bool
	eq       [15]float64
	resample float64
	tempo    float64
	wet      [wetCount]float64
	held     Filters
}

func glide(cur, target float64) float64 {
	next := cur + (target-cur)*rampAlpha
	if math.Abs(target-next) < rampSnap {
		return target
	}
	return next
}

func eqTargets(bands []Band) [15]float64 {
	var gains [15]float64
	for _, b := range bands {
		if b.Band >= 0 && b.Band < 15 {
			gains[b.Band] = math.Max(-0.25, math.Min(1, b.Gain))
		}
	}
	return gains
}

func wetTargets(f Filters) [wetCount]float64 {
	var w [wetCount]float64
	on := func(b bool) float64 {
		if b {
			return 1
		}
		return 0
	}
	w[wetKaraoke] = on(f.Karaoke != nil)
	w[wetLowPass] = on(f.LowPass != nil && f.LowPass.Smoothing > 1)
	w[wetVibrato] = on(f.Vibrato != nil && f.Vibrato.Frequency > 0)
	w[wetTremolo] = on(f.Tremolo != nil && f.Tremolo.Frequency > 0)
	w[wetRotation] = on(f.Rotation != nil && f.Rotation.RotationHz != 0)
	w[wetReverb] = on(f.Reverb != nil && f.Reverb.Mix > 0)
	return w
}

func (r *filterRamp) hold(t Filters) {
	if t.Karaoke != nil {
		r.held.Karaoke = t.Karaoke
	}
	if t.LowPass != nil {
		r.held.LowPass = t.LowPass
	}
	if t.Vibrato != nil {
		r.held.Vibrato = t.Vibrato
	}
	if t.Tremolo != nil {
		r.held.Tremolo = t.Tremolo
	}
	if t.Rotation != nil {
		r.held.Rotation = t.Rotation
	}
	if t.Reverb != nil {
		r.held.Reverb = t.Reverb
	}
}

func (r *filterRamp) advance(t Filters) {
	eq, wet := eqTargets(t.Equalizer), wetTargets(t)
	r.hold(t)
	if !r.ready {
		r.eq, r.wet = eq, wet
		r.resample, r.tempo = t.resampleFactor(), t.tempo()
		r.ready = true
		return
	}
	for i := range r.eq {
		r.eq[i] = glide(r.eq[i], eq[i])
	}
	for i := range r.wet {
		r.wet[i] = glide(r.wet[i], wet[i])
	}
	r.resample = glide(r.resample, t.resampleFactor())
	r.tempo = glide(r.tempo, t.tempo())
}

func (r *filterRamp) effective() Filters {
	var f Filters
	for i, g := range r.eq {
		if g != 0 {
			f.Equalizer = append(f.Equalizer, Band{Band: i, Gain: g})
		}
	}
	if r.wet[wetKaraoke] > 0 {
		f.Karaoke = r.held.Karaoke
	}
	if r.wet[wetLowPass] > 0 {
		f.LowPass = r.held.LowPass
	}
	if r.wet[wetVibrato] > 0 {
		f.Vibrato = r.held.Vibrato
	}
	if r.wet[wetTremolo] > 0 {
		f.Tremolo = r.held.Tremolo
	}
	if r.wet[wetRotation] > 0 {
		f.Rotation = r.held.Rotation
	}
	if r.wet[wetReverb] > 0 {
		f.Reverb = r.held.Reverb
	}
	return f
}

func (r *filterRamp) resampleFactor(fallback Filters) float64 {
	if !r.ready {
		return fallback.resampleFactor()
	}
	return r.resample
}

func (r *filterRamp) tempoFactor(fallback Filters) float64 {
	if !r.ready {
		return fallback.tempo()
	}
	return r.tempo
}
