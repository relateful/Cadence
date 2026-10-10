package player

import (
	"math"

	"github.com/greed/cadence/internal/audio"
)

var EqualizerBandsHz = [15]float64{25, 40, 63, 100, 160, 250, 400, 630, 1000, 1600, 2500, 4000, 6300, 10000, 16000}

type Band struct {
	Band int     `json:"band"`
	Gain float64 `json:"gain"`
}

type Timescale struct {
	Speed float64 `json:"speed"`
	Pitch float64 `json:"pitch"`
	Rate  float64 `json:"rate"`
}

type Oscillator struct {
	Frequency float64 `json:"frequency"`
	Depth     float64 `json:"depth"`
}

type Rotation struct {
	RotationHz float64 `json:"rotationHz"`
}

type LowPass struct {
	Smoothing float64 `json:"smoothing"`
}

type Karaoke struct {
	Level     float64 `json:"level"`
	MonoLevel float64 `json:"monoLevel"`
}

type Filters struct {
	Equalizer []Band      `json:"equalizer,omitempty"`
	Timescale *Timescale  `json:"timescale,omitempty"`
	Tremolo   *Oscillator `json:"tremolo,omitempty"`
	Vibrato   *Oscillator `json:"vibrato,omitempty"`
	Rotation  *Rotation   `json:"rotation,omitempty"`
	LowPass   *LowPass    `json:"lowPass,omitempty"`
	Karaoke   *Karaoke    `json:"karaoke,omitempty"`
	Reverb    *Reverb     `json:"reverb,omitempty"`
}

func DefaultFilters() Filters {
	return Filters{}
}

func positive(v, fallback float64) float64 {
	if v <= 0 {
		return fallback
	}
	return v
}

func (f Filters) resampleFactor() float64 {
	if f.Timescale == nil {
		return 1
	}
	return positive(f.Timescale.Rate, 1) * positive(f.Timescale.Pitch, 1)
}

func (f Filters) tempo() float64 {
	if f.Timescale == nil {
		return 1
	}
	return positive(f.Timescale.Speed, 1) / positive(f.Timescale.Pitch, 1)
}

func (f Filters) PositionRate() float64 {
	if f.Timescale == nil {
		return 1
	}
	return positive(f.Timescale.Speed, 1) * positive(f.Timescale.Rate, 1)
}

const vibratoMaxDelay = 480

type effects struct {
	eqL, eqR   [15]audio.Biquad
	eqGains    [15]float64
	eqReady    bool
	lowL, lowR float64
	phase      float64
	rotPhase   float64
	vibPhase   float64
	delayL     [vibratoMaxDelay]float64
	delayR     [vibratoMaxDelay]float64
	delayIdx   int
	rev        reverbState
}

func (e *effects) configureEq(bands []Band) bool {
	var gains [15]float64
	active := false
	for _, b := range bands {
		if b.Band >= 0 && b.Band < 15 {
			gains[b.Band] = math.Max(-0.25, math.Min(1, b.Gain))
			if gains[b.Band] != 0 {
				active = true
			}
		}
	}
	if !active {
		return false
	}
	if e.eqReady && gains == e.eqGains {
		return true
	}
	for i, hz := range EqualizerBandsHz {
		b0, b1, b2, a1, a2 := peaking(hz, gains[i])
		e.eqL[i].Coefficients(b0, b1, b2, a1, a2)
		e.eqR[i].Coefficients(b0, b1, b2, a1, a2)
	}
	e.eqGains, e.eqReady = gains, true
	return true
}

func peaking(hz, gain float64) (float64, float64, float64, float64, float64) {
	db := 20 * math.Log10(1+gain)
	if gain <= -1 {
		db = -40
	}
	a := math.Pow(10, db/40)
	w0 := 2 * math.Pi * hz / audio.SampleRate
	alpha := math.Sin(w0) / (2 * 1.1)
	cos := math.Cos(w0)
	a0 := 1 + alpha/a
	return (1 + alpha*a) / a0, (-2 * cos) / a0, (1 - alpha*a) / a0, (-2 * cos) / a0, (1 - alpha/a) / a0
}

func (f Filters) Active() bool {
	return len(f.Equalizer) > 0 || f.Tremolo != nil || f.Vibrato != nil || f.Rotation != nil || f.LowPass != nil || f.Karaoke != nil || f.Reverb != nil
}

func mix(dry, wet, amount float64) float64 {
	return dry + (wet-dry)*amount
}

func (e *effects) apply(f Filters, wet [wetCount]float64, out *audio.Frame) {
	if !f.Active() {
		return
	}
	eq := e.configureEq(f.Equalizer)
	dt := 1.0 / audio.SampleRate
	for i := 0; i < len(out); i += 2 {
		l, r := float64(out[i]), float64(out[i+1])
		if f.Karaoke != nil {
			level := math.Max(0, math.Min(1, f.Karaoke.Level)) * wet[wetKaraoke]
			mid := (l + r) / 2
			l -= mid * level
			r -= mid * level
		}
		if eq {
			for b := range e.eqL {
				l = e.eqL[b].Process(l)
				r = e.eqR[b].Process(r)
			}
		}
		if f.LowPass != nil && f.LowPass.Smoothing > 1 {
			e.lowL += (l - e.lowL) / f.LowPass.Smoothing
			e.lowR += (r - e.lowR) / f.LowPass.Smoothing
			l, r = mix(l, e.lowL, wet[wetLowPass]), mix(r, e.lowR, wet[wetLowPass])
		}
		if f.Vibrato != nil && f.Vibrato.Frequency > 0 {
			e.delayL[e.delayIdx], e.delayR[e.delayIdx] = l, r
			depth := math.Max(0, math.Min(1, f.Vibrato.Depth))
			lag := int((0.5 + 0.5*math.Sin(e.vibPhase)) * depth * (vibratoMaxDelay - 2))
			j := (e.delayIdx - lag + vibratoMaxDelay) % vibratoMaxDelay
			l, r = mix(l, e.delayL[j], wet[wetVibrato]), mix(r, e.delayR[j], wet[wetVibrato])
			e.delayIdx = (e.delayIdx + 1) % vibratoMaxDelay
			e.vibPhase += 2 * math.Pi * f.Vibrato.Frequency * dt
		}
		if f.Tremolo != nil && f.Tremolo.Frequency > 0 {
			depth := math.Max(0, math.Min(1, f.Tremolo.Depth)) * wet[wetTremolo]
			g := 1 - depth*(0.5+0.5*math.Sin(e.phase))
			l, r = l*g, r*g
			e.phase += 2 * math.Pi * f.Tremolo.Frequency * dt
		}
		if f.Rotation != nil && f.Rotation.RotationHz != 0 {
			angle := (math.Sin(e.rotPhase) + 1) * math.Pi / 4
			rl, rr := l*math.Cos(angle)*math.Sqrt2, r*math.Sin(angle)*math.Sqrt2
			l, r = mix(l, rl, wet[wetRotation]), mix(r, rr, wet[wetRotation])
			e.rotPhase += 2 * math.Pi * f.Rotation.RotationHz * dt
		}
		if f.Reverb != nil {
			l, r = e.rev.process(l, r, f.Reverb, wet[wetReverb])
		}
		out[i], out[i+1] = audio.Clip(l), audio.Clip(r)
	}
	e.phase = math.Mod(e.phase, 2*math.Pi)
	e.rotPhase = math.Mod(e.rotPhase, 2*math.Pi)
	e.vibPhase = math.Mod(e.vibPhase, 2*math.Pi)
}
