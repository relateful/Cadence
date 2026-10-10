package player

import (
	"math"

	"github.com/greed/cadence/internal/source"
)

const (
	normMaxCutDb   = -12.0
	normMaxBoostDb = 6.0
	normPeakCeil   = 0.891
	normFloorLufs  = -70.0
)

type Loudness struct {
	IntegratedLufs float64
	PeakAmplitude  float64
}

type LoudnessLookup func(t, playable source.Track) (Loudness, bool)

func NormalizationGain(l Loudness, targetLufs float64) float64 {
	if math.IsNaN(l.IntegratedLufs) || math.IsInf(l.IntegratedLufs, 0) || l.IntegratedLufs < normFloorLufs {
		return 1
	}
	db := min(normMaxBoostDb, max(normMaxCutDb, targetLufs-l.IntegratedLufs))
	gain := math.Pow(10, db/20)
	if l.PeakAmplitude > 0 {
		gain = min(gain, normPeakCeil/l.PeakAmplitude)
	}
	return gain
}

func (p *Player) levelDeck(d *Deck) {
	d.norm = 1
	if p.cfg.Loudness == nil {
		return
	}
	if l, ok := p.cfg.Loudness(d.Track, d.Playable); ok {
		d.norm, d.measured = NormalizationGain(l, p.cfg.TargetLufs), true
		return
	}
	if p.cfg.OnUnmeasured != nil {
		p.cfg.OnUnmeasured(d.Track)
	}
}
