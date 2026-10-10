package analysis

import (
	"math"
	"sort"

	"github.com/greed/cadence/internal/audio"
)

const (
	FloorLUFS         = -70.0
	subBlocksPerSec   = 10
	subBlocksPerBlock = 4
	absoluteGate      = -70.0
	relativeGateLU    = 10.0
	kWeightOffset     = -0.691
)

type Loudness struct {
	shelfL, shelfR, hpL, hpR audio.Biquad
	subBlocks                []float64
	sum                      float64
	filled                   int
	peak                     float64
}

func NewLoudness() *Loudness {
	l := &Loudness{}
	for _, b := range []*audio.Biquad{&l.shelfL, &l.shelfR} {
		b.Coefficients(1.53512485958697, -2.69169618940638, 1.19839281085285, -1.69065929318241, 0.73248077421585)
	}
	for _, b := range []*audio.Biquad{&l.hpL, &l.hpR} {
		b.Coefficients(1.0, -2.0, 1.0, -1.99004745483398, 0.99007225036621)
	}
	return l
}

func (l *Loudness) Feed(f *audio.Frame) {
	per := audio.SampleRate / subBlocksPerSec
	for i := 0; i < len(f); i += 2 {
		left, right := float64(f[i])/32768, float64(f[i+1])/32768
		l.peak = math.Max(l.peak, math.Max(math.Abs(left), math.Abs(right)))
		wl := l.hpL.Process(l.shelfL.Process(left))
		wr := l.hpR.Process(l.shelfR.Process(right))
		l.sum += wl*wl + wr*wr
		if l.filled++; l.filled == per {
			l.subBlocks = append(l.subBlocks, l.sum)
			l.sum, l.filled = 0, 0
		}
	}
}

func (l *Loudness) Peak() float64 {
	return l.peak
}

func loudnessOf(power float64) float64 {
	if power <= 0 {
		return math.Inf(-1)
	}
	return kWeightOffset + 10*math.Log10(power)
}

func (l *Loudness) Integrated() float64 {
	if len(l.subBlocks) < subBlocksPerBlock {
		return FloorLUFS
	}
	blockSamples := float64(audio.SampleRate / subBlocksPerSec * subBlocksPerBlock)
	var above []float64
	for start := 0; start <= len(l.subBlocks)-subBlocksPerBlock; start++ {
		sum := 0.0
		for o := 0; o < subBlocksPerBlock; o++ {
			sum += l.subBlocks[start+o]
		}
		if p := sum / blockSamples; loudnessOf(p) > absoluteGate {
			above = append(above, p)
		}
	}
	if len(above) == 0 {
		return FloorLUFS
	}
	gate := loudnessOf(mean(above)) - relativeGateLU
	var gated []float64
	for _, p := range above {
		if loudnessOf(p) > gate {
			gated = append(gated, p)
		}
	}
	if len(gated) == 0 {
		return FloorLUFS
	}
	return math.Max(FloorLUFS, loudnessOf(mean(gated)))
}

const (
	silenceLevel  = 0.05
	fullRatio     = 0.85
	maxIntroShare = 0.3
	minOutroShare = 0.5
	sustainPoints = 3
)

func clampInt(v, lo, hi int) int {
	if hi <= lo {
		return lo
	}
	return max(lo, min(v, hi))
}

func Snap(lines []int, t int) int {
	if len(lines) == 0 {
		return t
	}
	return lines[nearestIndex(lines, t)]
}

func DetectMixRegions(energy []EnergyPoint, vocals []VocalActivity, phrases []int, durationMs int) MixRegions {
	whole := MixRegions{OutroStartMs: durationMs, OutroEndMs: durationMs}
	if len(energy) == 0 || durationMs <= 0 {
		return whole
	}
	first, last := -1, -1
	for i, p := range energy {
		if p.Value >= silenceLevel {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	if first < 0 || last <= first {
		return whole
	}
	start := energy[first].TimeMs
	end := min(durationMs, energy[last].TimeMs)
	values := make([]float64, 0, last-first+1)
	for _, p := range energy[first : last+1] {
		values = append(values, p.Value)
	}
	sort.Float64s(values)
	full := values[len(values)/2] * fullRatio

	introEnd := clampInt(introEndOf(energy, vocals, first, last, full, start), start, start+int(float64(durationMs)*maxIntroShare))
	outroStart := clampInt(outroStartOf(energy, first, last, full, end), int(float64(durationMs)*minOutroShare), end)
	si := clampInt(Snap(phrases, introEnd), start, end)
	so := clampInt(Snap(phrases, outroStart), start, end)
	if si >= so {
		return MixRegions{IntroStartMs: start, IntroEndMs: start, OutroStartMs: end, OutroEndMs: end}
	}
	return MixRegions{IntroStartMs: start, IntroEndMs: si, OutroStartMs: so, OutroEndMs: end}
}

func introEndOf(energy []EnergyPoint, vocals []VocalActivity, first, last int, full float64, start int) int {
	loud := start
	for i := first; i <= last-sustainPoints; i++ {
		sustained := true
		for o := 0; o < sustainPoints; o++ {
			if energy[i+o].Value < full {
				sustained = false
				break
			}
		}
		if sustained {
			loud = energy[i].TimeMs
			break
		}
	}
	if len(vocals) == 0 {
		return loud
	}
	return min(loud, max(start, vocals[0].StartMs))
}

func outroStartOf(energy []EnergyPoint, first, last int, full float64, end int) int {
	for i := last; i >= first; i-- {
		if energy[i].Value >= full {
			if i >= last {
				return end
			}
			return energy[i+1].TimeMs
		}
	}
	return end
}

const (
	KernelLags       = 32
	MinSegmentMs     = 8000
	noveltyThreshold = 1.4
	repeatThreshold  = 0.86
	edgeEnergyRatio  = 0.8
	chorusRepeat     = 0.3
	quietLevel       = 0.35
	loudLevel        = 0.7
	buildSlope       = 0.15
	smoothRadius     = 4
)

func norm(v []float64) float64 {
	s := 0.0
	for _, x := range v {
		s += x * x
	}
	return math.Sqrt(s)
}

func SimilarityMatrix(features [][]float64) [][]float64 {
	n := len(features)
	out := make([][]float64, n)
	norms := make([]float64, n)
	for i := range features {
		out[i] = make([]float64, n)
		norms[i] = norm(features[i])
	}
	for i := 0; i < n; i++ {
		out[i][i] = 1
		for j := i + 1; j < n; j++ {
			v := 0.0
			if norms[i] > 0 && norms[j] > 0 {
				dot := 0.0
				for k := 0; k < min(len(features[i]), len(features[j])); k++ {
					dot += features[i][k] * features[j][k]
				}
				v = clamp(dot/(norms[i]*norms[j]), 0, 1)
			}
			out[i][j], out[j][i] = v, v
		}
	}
	return out
}

func checkerboard(lags int) [][]float64 {
	size := lags * 2
	sigma := float64(lags) / 2
	k := make([][]float64, size)
	for a := 0; a < size; a++ {
		k[a] = make([]float64, size)
		for b := 0; b < size; b++ {
			x, y := float64(a-lags)+0.5, float64(b-lags)+0.5
			sign := -1.0
			if (x < 0) == (y < 0) {
				sign = 1
			}
			k[a][b] = sign * math.Exp(-(x*x+y*y)/(2*sigma*sigma))
		}
	}
	return k
}

func Novelty(ssm [][]float64, lags int) []float64 {
	n := len(ssm)
	out := make([]float64, n)
	if n < lags*2 {
		return out
	}
	k := checkerboard(lags)
	for c := lags; c < n-lags; c++ {
		sum := 0.0
		for a := -lags; a < lags; a++ {
			for b := -lags; b < lags; b++ {
				sum += k[a+lags][b+lags] * ssm[c+a][c+b]
			}
		}
		out[c] = math.Max(0, sum)
	}
	return out
}

func NoveltyBoundaries(novelty []float64, msPerStep, durationMs int) []int {
	bounds := []int{0}
	var positive []float64
	for _, v := range novelty {
		if v > 0 {
			positive = append(positive, v)
		}
	}
	if len(positive) == 0 || msPerStep <= 0 {
		return append(bounds, durationMs)
	}
	threshold := mean(positive) * noveltyThreshold
	for i := 1; i < len(novelty)-1; i++ {
		if novelty[i] < threshold || novelty[i] < novelty[i-1] || novelty[i] < novelty[i+1] {
			continue
		}
		ms := i * msPerStep
		if ms-bounds[len(bounds)-1] >= MinSegmentMs && durationMs-ms >= MinSegmentMs {
			bounds = append(bounds, ms)
		}
	}
	return append(bounds, durationMs)
}

type span struct{ from, to int }

func spanSimilarity(ssm [][]float64, a, b span) float64 {
	length := min(a.to-a.from, b.to-b.from)
	if length <= 0 {
		return 0
	}
	sum, count := 0.0, 0
	for o := 0; o <= length; o++ {
		x, y := a.from+o, b.from+o
		if x >= len(ssm) || y >= len(ssm) {
			break
		}
		sum += ssm[x][y]
		count++
	}
	if count == 0 {
		return 0
	}
	return sum / float64(count)
}

func repetition(ssm [][]float64, spans []span) []float64 {
	out := make([]float64, len(spans))
	for i := range spans {
		repeats := 0
		for j := range spans {
			if i != j && spanSimilarity(ssm, spans[i], spans[j]) >= repeatThreshold {
				repeats++
			}
		}
		if len(spans) > 1 {
			out[i] = float64(repeats) / float64(len(spans)-1)
		}
	}
	return out
}

func smooth(values []float64) []float64 {
	out := make([]float64, len(values))
	for i := range values {
		sum, count := 0.0, 0
		for j := max(0, i-smoothRadius); j <= min(len(values)-1, i+smoothRadius); j++ {
			sum += values[j]
			count++
		}
		out[i] = sum / float64(count)
	}
	return out
}

func energyBoundaries(smoothed []float64, energy []EnergyPoint, durationMs int) []int {
	novelty := make([]float64, len(smoothed))
	for i := 1; i < len(smoothed); i++ {
		novelty[i] = math.Abs(smoothed[i] - smoothed[i-1])
	}
	threshold := mean(novelty) * 2.5
	bounds := []int{0}
	for i := 1; i < len(novelty); i++ {
		if novelty[i] > threshold {
			ms := energy[i].TimeMs
			if ms-bounds[len(bounds)-1] >= MinSegmentMs && durationMs-ms >= MinSegmentMs {
				bounds = append(bounds, ms)
			}
		}
	}
	return append(bounds, durationMs)
}

func classify(index, total int, level, slope, meanLevel, repeat float64) string {
	quiet := level <= meanLevel*edgeEnergyRatio
	switch {
	case index == 0 && quiet:
		return "intro"
	case index == total-1 && quiet:
		return "outro"
	case slope > buildSlope:
		return "build"
	case repeat >= chorusRepeat && level >= loudLevel:
		return "chorus"
	case repeat >= chorusRepeat:
		return "verse"
	case level <= quietLevel:
		return "breakdown"
	case level >= loudLevel:
		return "drop"
	case repeat > 0:
		return "verse"
	}
	return "bridge"
}

func beatIndexAt(beats []int, ms int) int {
	if len(beats) == 0 {
		return 0
	}
	i := sort.SearchInts(beats, ms)
	return min(i, len(beats)-1)
}

func Segment(energy []EnergyPoint, beats []int, durationMs int, features [][]float64, msPerFeature int) []Section {
	if len(energy) == 0 || durationMs <= 0 {
		return nil
	}
	raw := make([]float64, len(energy))
	for i, p := range energy {
		raw[i] = p.Value
	}
	smoothed := smooth(raw)
	peak := math.Max(maxOf(smoothed), 1e-6)
	meanLevel := math.Max(mean(smoothed)/peak, 1e-6)

	var ssm [][]float64
	if len(features) >= KernelLags*2 && msPerFeature > 0 {
		ssm = SimilarityMatrix(features)
	}
	var bounds []int
	if ssm != nil {
		bounds = NoveltyBoundaries(Novelty(ssm, KernelLags), msPerFeature, durationMs)
	} else {
		bounds = energyBoundaries(smoothed, energy, durationMs)
	}
	repeats := make([]float64, len(bounds)-1)
	if ssm != nil {
		spans := make([]span, 0, len(bounds)-1)
		for i := 0; i < len(bounds)-1; i++ {
			from := clampInt(bounds[i]/msPerFeature, 0, len(ssm)-1)
			to := clampInt(bounds[i+1]/msPerFeature, from, len(ssm)-1)
			spans = append(spans, span{from, to})
		}
		repeats = repetition(ssm, spans)
	}
	sections := make([]Section, 0, len(bounds)-1)
	for i := 0; i < len(bounds)-1; i++ {
		start, end := bounds[i], bounds[i+1]
		sum, count, firstIdx, lastIdx := 0.0, 0, -1, -1
		for j, p := range energy {
			if p.TimeMs >= start && p.TimeMs < end {
				sum += smoothed[j]
				count++
				if firstIdx < 0 {
					firstIdx = j
				}
				lastIdx = j
			}
		}
		level := 0.0
		if count > 0 {
			level = sum / float64(count) / peak
		}
		slope := 0.0
		if firstIdx >= 0 && lastIdx > firstIdx {
			slope = smoothed[lastIdx] - smoothed[firstIdx]
		}
		sections = append(sections, Section{
			Type:      classify(i, len(bounds)-1, level, slope, meanLevel, repeats[i]),
			StartMs:   start,
			EndMs:     end,
			Energy:    clamp(level, 0, 1),
			BeatStart: beatIndexAt(beats, start),
			BeatEnd:   beatIndexAt(beats, end),
		})
	}
	return sections
}
