package audio

import "math"

type TransitionProfile struct {
	BassSwapAt     *float64 `json:"bassSwapAt,omitempty"`
	BassSwapRamp   float64  `json:"bassSwapRamp,omitempty"`
	SweepOutFromHz float64  `json:"sweepOutFromHz,omitempty"`
	SweepOutToHz   float64  `json:"sweepOutToHz,omitempty"`
	SweepInFromHz  float64  `json:"sweepInFromHz,omitempty"`
	SweepInToHz    float64  `json:"sweepInToHz,omitempty"`
}

const defaultBassSwapRamp = 0.12

func clamp01(v float64) float64 {
	return math.Min(1, math.Max(0, v))
}

func (p TransitionProfile) ramp() float64 {
	if p.BassSwapRamp <= 0 {
		return defaultBassSwapRamp
	}
	return p.BassSwapRamp
}

func (p TransitionProfile) SwapsBass() bool {
	return p.BassSwapAt != nil
}

func (p TransitionProfile) SweepsOut() bool {
	return p.SweepOutToHz > MinSweepHz || p.SweepOutFromHz > MinSweepHz
}

func (p TransitionProfile) SweepsIn() bool {
	return p.SweepInToHz > MinSweepHz || p.SweepInFromHz > MinSweepHz
}

func (p TransitionProfile) OutgoingBassGain(progress float64) float64 {
	if p.BassSwapAt == nil || progress <= *p.BassSwapAt {
		return 1
	}
	return clamp01(1 - (progress-*p.BassSwapAt)/p.ramp())
}

func (p TransitionProfile) IncomingBassGain(progress float64) float64 {
	if p.BassSwapAt == nil {
		return 1
	}
	if progress <= *p.BassSwapAt {
		return 0
	}
	return clamp01((progress - *p.BassSwapAt) / p.ramp())
}

func (p TransitionProfile) OutgoingSweepHz(progress float64) float64 {
	if !p.SweepsOut() {
		return 0
	}
	return p.SweepOutFromHz + (p.SweepOutToHz-p.SweepOutFromHz)*progress
}

func (p TransitionProfile) IncomingSweepHz(progress float64) float64 {
	if !p.SweepsIn() {
		return 0
	}
	return p.SweepInFromHz + (p.SweepInToHz-p.SweepInFromHz)*progress
}
