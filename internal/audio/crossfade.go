package audio

import (
	"fmt"
	"math"
	"strings"
)

type FadeCurve int

const (
	CurveLinear FadeCurve = iota
	CurveEqualPower
)

func ParseCurve(raw string) (FadeCurve, error) {
	switch strings.ToUpper(strings.ReplaceAll(raw, "-", "_")) {
	case "", "EQUAL_POWER":
		return CurveEqualPower, nil
	case "LINEAR":
		return CurveLinear, nil
	}
	return 0, fmt.Errorf("unknown curve %q", raw)
}

type Crossfade struct {
	curve   FadeCurve
	total   int
	frame   int
	OutGain float64
	InGain  float64
}

func NewCrossfade(durationMs int, curve FadeCurve) *Crossfade {
	return &Crossfade{curve: curve, total: max(1, durationMs/FrameMs), OutGain: 1}
}

func (c *Crossfade) Finished() bool {
	return c.frame >= c.total
}

func (c *Crossfade) Progress() float64 {
	return math.Min(1, math.Max(0, float64(c.frame)/float64(c.total)))
}

func (c *Crossfade) DurationMs() int {
	return c.total * FrameMs
}

func (c *Crossfade) Advance() {
	if c.Finished() {
		c.OutGain, c.InGain = 0, 1
		return
	}
	c.frame++
	p := float64(c.frame) / float64(c.total)
	switch c.curve {
	case CurveLinear:
		c.OutGain, c.InGain = 1-p, p
	default:
		c.OutGain, c.InGain = math.Cos(p*math.Pi/2), math.Sin(p*math.Pi/2)
	}
}
