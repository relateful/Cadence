package analysis

import (
	"errors"
	"math"
)

var ErrFFTSize = errors.New("fft size must be a power of two")

func FFT(re, im []float64) error {
	n := len(re)
	if n&(n-1) != 0 {
		return ErrFFTSize
	}
	j := 0
	for i := 1; i < n; i++ {
		bit := n >> 1
		for ; j&bit != 0; bit >>= 1 {
			j ^= bit
		}
		j |= bit
		if i < j {
			re[i], re[j] = re[j], re[i]
			im[i], im[j] = im[j], im[i]
		}
	}
	for size := 2; size <= n; size <<= 1 {
		ang := -2 * math.Pi / float64(size)
		wr, wi := math.Cos(ang), math.Sin(ang)
		half := size / 2
		for i := 0; i < n; i += size {
			cr, ci := 1.0, 0.0
			for k := 0; k < half; k++ {
				a, b := i+k, i+k+half
				vr := re[b]*cr - im[b]*ci
				vi := re[b]*ci + im[b]*cr
				re[b], im[b] = re[a]-vr, im[a]-vi
				re[a], im[a] = re[a]+vr, im[a]+vi
				cr, ci = cr*wr-ci*wi, cr*wi+ci*wr
			}
		}
	}
	return nil
}

type Tempo struct {
	BPM          float64
	Confidence   float64
	PeriodFrames float64
	PhaseFrames  float64
}

const (
	minBPM          = 60.0
	maxBPM          = 200.0
	preferredMinBPM = 90.0
	preferredMaxBPM = 180.0
)

func mean(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := 0.0
	for _, x := range v {
		s += x
	}
	return s / float64(len(v))
}

func maxOf(v []float64) float64 {
	m := math.Inf(-1)
	for _, x := range v {
		m = math.Max(m, x)
	}
	return m
}

func clamp(v, lo, hi float64) float64 {
	return math.Max(lo, math.Min(hi, v))
}

func autocorr(env []float64, lag int) float64 {
	sum := 0.0
	for i := lag; i < len(env); i++ {
		sum += env[i] * env[i-lag]
	}
	return sum / float64(len(env)-lag)
}

func EstimateTempo(envelope []float64, frameRate float64) Tempo {
	if len(envelope) < 16 {
		return Tempo{}
	}
	m := mean(envelope)
	centered := make([]float64, len(envelope))
	for i, v := range envelope {
		centered[i] = math.Max(0, v-m)
	}
	minLag := max(2, int(60*frameRate/maxBPM))
	maxLag := min(len(centered)/2, int(60*frameRate/minBPM))
	if maxLag <= minLag {
		return Tempo{}
	}
	bestLag, bestScore, scoreSum := minLag, math.Inf(-1), 0.0
	for lag := minLag; lag <= maxLag; lag++ {
		s := autocorr(centered, lag)
		scoreSum += s
		if s > bestScore {
			bestScore, bestLag = s, lag
		}
	}
	avg := scoreSum / float64(maxLag-minLag+1)
	confidence := 0.0
	if bestScore > 0 {
		confidence = clamp((bestScore-avg)/bestScore, 0, 1)
	}
	period := refinePeriod(centered, bestLag)
	bpm := 0.0
	if period > 0 {
		bpm = 60 * frameRate / period
	}
	for bpm >= 0.1 && bpm <= preferredMinBPM && bpm*2 <= preferredMaxBPM {
		bpm, period = bpm*2, period/2
	}
	for bpm > preferredMaxBPM {
		bpm, period = bpm/2, period*2
	}
	return Tempo{BPM: bpm, Confidence: confidence, PeriodFrames: period, PhaseFrames: beatPhase(centered, period)}
}

func refinePeriod(env []float64, lag int) float64 {
	if lag <= 1 || lag >= len(env)-1 {
		return float64(lag)
	}
	prev, cur, next := autocorr(env, lag-1), autocorr(env, lag), autocorr(env, lag+1)
	denom := prev - 2*cur + next
	if denom == 0 {
		return float64(lag)
	}
	offset := 0.5 * (prev - next) / denom
	if offset > -1 && offset < 1 {
		return float64(lag) + offset
	}
	return float64(lag)
}

func beatPhase(env []float64, period float64) float64 {
	if period <= 0 {
		return 0
	}
	best, bestSum := 0.0, math.Inf(-1)
	for step := 0; step < max(1, int(period)); step++ {
		sum := 0.0
		for pos := float64(step); pos < float64(len(env)); pos += period {
			sum += env[int(pos)]
		}
		if sum > bestSum {
			bestSum, best = sum, float64(step)
		}
	}
	return best
}

var (
	majorProfile = [12]float64{6.35, 2.23, 3.48, 2.33, 4.38, 4.09, 2.52, 5.19, 2.39, 3.66, 2.29, 2.88}
	minorProfile = [12]float64{6.33, 2.68, 3.52, 5.38, 2.60, 3.53, 2.54, 4.75, 3.98, 2.69, 3.34, 3.17}
	pitchNames   = [12]string{"C", "C#", "D", "D#", "E", "F", "F#", "G", "G#", "A", "A#", "B"}
	majorCamelot = [12]string{"8B", "3B", "10B", "5B", "12B", "7B", "2B", "9B", "4B", "11B", "6B", "1B"}
	minorCamelot = [12]string{"5A", "12A", "7A", "2A", "9A", "4A", "11A", "6A", "1A", "8A", "3A", "10A"}
)

func correlate(chroma []float64, profile [12]float64, rotation int) float64 {
	cm, pm := 0.0, 0.0
	for i := 0; i < 12; i++ {
		cm += chroma[i]
		pm += profile[i]
	}
	cm, pm = cm/12, pm/12
	num, cv, pv := 0.0, 0.0, 0.0
	for i := 0; i < 12; i++ {
		c := chroma[(i+rotation)%12] - cm
		p := profile[i] - pm
		num += c * p
		cv += c * c
		pv += p * p
	}
	denom := math.Sqrt(cv * pv)
	if denom <= 0 {
		return 0
	}
	return num / denom
}

func DetectKey(chroma [12]float64) Harmony {
	total := 0.0
	for _, c := range chroma {
		total += c
	}
	if total <= 0 {
		return Harmony{Camelot: "8A", Key: "A", Scale: "minor"}
	}
	norm := make([]float64, 12)
	for i := range chroma {
		norm[i] = chroma[i] / total
	}
	best, second := math.Inf(-1), math.Inf(-1)
	bestPC, bestMajor := 0, true
	for pc := 0; pc < 12; pc++ {
		for _, major := range []bool{true, false} {
			profile := minorProfile
			if major {
				profile = majorProfile
			}
			s := correlate(norm, profile, pc)
			if s > best {
				second, best, bestPC, bestMajor = best, s, pc, major
			} else if s > second {
				second = s
			}
		}
	}
	margin := 0.0
	if best > 0 && !math.IsInf(second, -1) {
		margin = clamp((best-second)/best, 0, 1)
	}
	h := Harmony{Key: pitchNames[bestPC], Confidence: clamp(clamp(best, 0, 1)*0.5+margin*0.5, 0, 1)}
	if bestMajor {
		h.Camelot, h.Scale = majorCamelot[bestPC], "major"
	} else {
		h.Camelot, h.Scale = minorCamelot[bestPC], "minor"
	}
	return h
}

type Meter struct {
	BeatsPerBar int
	Phase       int
	Confidence  float64
}

const (
	preferFour   = 1.1
	minMeterBeat = 8
)

func floorMod(a, b int) int {
	return ((a % b) + b) % b
}

func DetectMeter(accents []float64) Meter {
	fallback := Meter{BeatsPerBar: 4}
	if len(accents) < minMeterBeat {
		return fallback
	}
	best := fallback
	for _, candidate := range []int{4, 3} {
		m := meterPhase(accents, candidate)
		weighted := m.Confidence
		if candidate == 4 {
			weighted *= preferFour
		}
		if weighted > best.Confidence {
			best = Meter{BeatsPerBar: candidate, Phase: m.Phase, Confidence: weighted}
		}
	}
	if best.Confidence <= 0 {
		return fallback
	}
	return best
}

func AccentsAtBeats(envelope []float64, beatsMs []int, frameRate float64) []float64 {
	if len(envelope) == 0 || len(beatsMs) == 0 {
		return nil
	}
	out := make([]float64, len(beatsMs))
	for i, b := range beatsMs {
		centre := int(float64(b) / 1000 * frameRate)
		for off := -1; off <= 1; off++ {
			if at := centre + off; at >= 0 && at < len(envelope) && envelope[at] > out[i] {
				out[i] = envelope[at]
			}
		}
	}
	return out
}

func meterPhase(accents []float64, beatsPerBar int) Meter {
	total := mean(accents)
	if total <= 0 {
		return Meter{BeatsPerBar: beatsPerBar}
	}
	bestPhase, bestContrast := 0, math.Inf(-1)
	for phase := 0; phase < beatsPerBar; phase++ {
		on, onN, off, offN := 0.0, 0, 0.0, 0
		for i, a := range accents {
			if floorMod(i-phase, beatsPerBar) == 0 {
				on += a
				onN++
			} else {
				off += a
				offN++
			}
		}
		if onN == 0 || offN == 0 {
			continue
		}
		if c := on/float64(onN) - off/float64(offN); c > bestContrast {
			bestContrast, bestPhase = c, phase
		}
	}
	return Meter{BeatsPerBar: beatsPerBar, Phase: bestPhase, Confidence: clamp(bestContrast/total, 0, 1)}
}

const (
	LongPhraseBars  = 16
	ShortPhraseBars = 8
	longPhraseShare = 0.75
)

type Phrases struct {
	Bars      int
	PhaseBar  int
	PhrasesMs []int
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func nearestIndex(values []int, target int) int {
	best, delta := 0, absInt(values[0]-target)
	for i, v := range values {
		if d := absInt(v - target); d < delta {
			best, delta = i, d
		}
	}
	return best
}

func DetectPhrases(downbeatsMs, boundariesMs []int) Phrases {
	if len(downbeatsMs) == 0 {
		return Phrases{Bars: LongPhraseBars}
	}
	bars := make([]int, len(boundariesMs))
	for i, b := range boundariesMs {
		bars[i] = nearestIndex(downbeatsMs, b)
	}
	longPhase, longScore := phrasePhase(bars, LongPhraseBars)
	shortPhase, shortScore := phrasePhase(bars, ShortPhraseBars)
	length, phase := ShortPhraseBars, shortPhase
	if shortScore == 0 || float64(longScore) >= float64(shortScore)*longPhraseShare {
		length, phase = LongPhraseBars, longPhase
	}
	var lines []int
	for i := phase; i < len(downbeatsMs); i += length {
		lines = append(lines, downbeatsMs[i])
	}
	return Phrases{Bars: length, PhaseBar: phase, PhrasesMs: lines}
}

func phrasePhase(bars []int, length int) (int, int) {
	if len(bars) == 0 {
		return 0, 0
	}
	best, bestScore := 0, -1
	for phase := 0; phase < length; phase++ {
		score := 0
		for _, b := range bars {
			if floorMod(b-phase, length) == 0 {
				score++
			}
		}
		if score > bestScore {
			best, bestScore = phase, score
		}
	}
	return best, bestScore
}

const (
	vocalLowHz       = 200.0
	vocalHighHz      = 4000.0
	minF0Hz          = 80.0
	maxF0Hz          = 1000.0
	vibratoLow       = 4.0
	vibratoHigh      = 7.0
	harmonics        = 4
	semitone         = 100.0
	maxVibratoFrames = 1024
)

func VocalRatio(mid, side []float64, binHz float64) float64 {
	centre, total := 0.0, 0.0
	for b := range mid {
		hz := float64(b) * binHz
		total += mid[b]
		if hz >= vocalLowHz && hz <= vocalHighHz {
			centre += math.Max(0, mid[b]-side[b])
		}
	}
	if total <= 0 {
		return 0
	}
	return clamp(centre/total, 0, 1)
}

func DominantF0(mag []float64, binHz float64) float64 {
	if len(mag) == 0 || binHz <= 0 {
		return 0
	}
	low := max(1, int(minF0Hz/binHz))
	high := min(len(mag)/harmonics-1, int(maxF0Hz/binHz))
	if high <= low {
		return 0
	}
	bestBin, bestScore := 0, 0.0
	for b := low; b <= high; b++ {
		product := mag[b]
		for h := 2; h <= harmonics && b*h < len(mag); h++ {
			product *= mag[b*h]
		}
		if product > bestScore {
			bestScore, bestBin = product, b
		}
	}
	if bestScore <= 0 {
		return 0
	}
	return float64(bestBin) * binHz
}

func cents(from, to float64) float64 {
	return 1200 * math.Log2(to/from)
}

func Continuity(f0 []float64, from, to int) float64 {
	if to-from < 2 {
		return 0
	}
	steady, compared := 0, 0
	for i := max(1, from); i < min(to, len(f0)); i++ {
		if f0[i-1] <= 0 || f0[i] <= 0 {
			continue
		}
		compared++
		if math.Abs(cents(f0[i-1], f0[i])) <= semitone {
			steady++
		}
	}
	if compared == 0 {
		return 0
	}
	return float64(steady) / float64(compared)
}

func VibratoStrength(f0 []float64, frameRate float64, from, to int) float64 {
	start := max(0, from)
	end := min(min(to, len(f0)), start+maxVibratoFrames)
	length := end - start
	if length < 8 || frameRate <= 0 {
		return 0
	}
	contour := make([]float64, length)
	sum, voiced := 0.0, 0
	for i := 0; i < length; i++ {
		if v := f0[start+i]; v > 0 {
			contour[i] = 1200 * math.Log2(v/minF0Hz)
			sum += contour[i]
			voiced++
		}
	}
	if voiced < length/2 {
		return 0
	}
	m := sum / float64(voiced)
	band, total := 0.0, 0.0
	step := frameRate / float64(length)
	for k := 1; k < length/2; k++ {
		hz := float64(k) * step
		re, im := 0.0, 0.0
		for n := 0; n < length; n++ {
			if f0[start+n] <= 0 {
				continue
			}
			angle := -2 * math.Pi * float64(k) * float64(n) / float64(length)
			c := contour[n] - m
			re += c * math.Cos(angle)
			im += c * math.Sin(angle)
		}
		p := re*re + im*im
		total += p
		if hz >= vibratoLow && hz <= vibratoHigh {
			band += p
		}
	}
	if total <= 0 {
		return 0
	}
	return clamp(band/total, 0, 1)
}
