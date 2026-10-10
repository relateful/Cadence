package analysis

import (
	"math"
	"sync"

	"github.com/greed/cadence/internal/audio"
)

const (
	window           = 2048
	hop              = 512
	bins             = window / 2
	timbreBands      = 8
	featureSize      = 12 + timbreBands
	framesPerFeature = 47
	minVocalRatio    = 0.12
	minVocalMs       = 1000
	minContinuity    = 0.6
	minVibrato       = 0.25
	maxBeats         = 20000
	chromaLowHz      = 65.0
	chromaHighHz     = 5000.0
)

var (
	frameRate = float64(audio.SampleRate) / hop
	binHz     = float64(audio.SampleRate) / window
	hannWin   = func() []float64 {
		w := make([]float64, window)
		for i := range w {
			w[i] = 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(window-1))
		}
		return w
	}()
)

type Analyzer struct {
	source, identifier string
	onDone             func(*Fingerprint)

	mid, side        [window]float64
	filled           int
	re, im, sre, sim [window]float64
	mag, sideMag     [bins]float64
	prev             [bins]float64

	flux, rms, centroid, vocal, f0 []float64
	chroma                         [12]float64
	features                       [][]float64
	acc                            [featureSize]float64
	accCount                       int
	loud                           *Loudness
	samples                        int64

	once     sync.Once
	finished *Fingerprint
}

func NewAnalyzer(source, identifier string, onDone func(*Fingerprint)) *Analyzer {
	return &Analyzer{source: source, identifier: identifier, onDone: onDone, loud: NewLoudness()}
}

func pitchClass(hz float64) int {
	midi := 69 + 12*math.Log2(hz/440)
	if math.IsNaN(midi) || midi < 0 {
		return -1
	}
	return floorMod(int(math.Round(midi)), 12)
}

func (a *Analyzer) Feed(f *audio.Frame) {
	if a.finished != nil {
		return
	}
	a.loud.Feed(f)
	for i := 0; i < len(f); i += 2 {
		l, r := float64(f[i])/32768, float64(f[i+1])/32768
		a.mid[a.filled], a.side[a.filled] = (l+r)*0.5, (l-r)*0.5
		a.filled++
		if a.filled == window {
			a.process()
			copy(a.mid[:], a.mid[hop:])
			copy(a.side[:], a.side[hop:])
			a.filled = window - hop
		}
	}
	a.samples += int64(len(f) / audio.Channels)
}

func (a *Analyzer) process() {
	for i := 0; i < window; i++ {
		a.re[i], a.im[i] = a.mid[i]*hannWin[i], 0
		a.sre[i], a.sim[i] = a.side[i]*hannWin[i], 0
	}
	FFT(a.re[:], a.im[:])
	FFT(a.sre[:], a.sim[:])
	energy, weighted, magSum, fluxSum := 0.0, 0.0, 0.0, 0.0
	for b := 0; b < bins; b++ {
		m := math.Hypot(a.re[b], a.im[b])
		a.mag[b] = m
		a.sideMag[b] = math.Hypot(a.sre[b], a.sim[b])
		energy += m * m
		magSum += m
		hz := float64(b) * binHz
		weighted += hz * m
		if d := m - a.prev[b]; d > 0 {
			fluxSum += d
		}
		if hz >= chromaLowHz && hz <= chromaHighHz && m > 0 {
			if pc := pitchClass(hz); pc >= 0 {
				a.chroma[pc] += m
			}
		}
	}
	a.prev = a.mag
	a.flux = append(a.flux, fluxSum)
	a.rms = append(a.rms, math.Sqrt(energy/bins))
	c := 0.0
	if magSum > 0 {
		c = weighted / magSum
	}
	a.centroid = append(a.centroid, c)
	a.vocal = append(a.vocal, VocalRatio(a.mag[:], a.sideMag[:], binHz))
	a.f0 = append(a.f0, DominantF0(a.mag[:], binHz))
	a.accumulate(magSum)
}

func (a *Analyzer) accumulate(magSum float64) {
	if magSum > 0 {
		for b := 0; b < bins; b++ {
			hz := float64(b) * binHz
			if hz < chromaLowHz || hz > chromaHighHz {
				continue
			}
			if pc := pitchClass(hz); pc >= 0 {
				a.acc[pc] += a.mag[b] / magSum
			}
		}
		for band := 0; band < timbreBands; band++ {
			from, to := bins*band/timbreBands, bins*(band+1)/timbreBands
			sum := 0.0
			for b := from; b < to; b++ {
				sum += a.mag[b]
			}
			a.acc[12+band] += sum / magSum
		}
	}
	if a.accCount++; a.accCount < framesPerFeature {
		return
	}
	v := make([]float64, featureSize)
	for i := range v {
		v[i] = a.acc[i] / float64(a.accCount)
	}
	a.features = append(a.features, v)
	a.acc, a.accCount = [featureSize]float64{}, 0
}

func toMs(frame float64) int {
	return int(frame / frameRate * 1000)
}

func (a *Analyzer) Finish() {
	a.once.Do(func() {
		fp := a.Result()
		if a.onDone != nil {
			a.onDone(fp)
		}
	})
}

func (a *Analyzer) Result() *Fingerprint {
	if a.finished != nil {
		return a.finished
	}
	durationMs := int(a.samples * 1000 / audio.SampleRate)
	tempo := EstimateTempo(a.flux, frameRate)
	beats := a.beats(tempo, durationMs)
	meter := DetectMeter(AccentsAtBeats(a.flux, beats, frameRate))
	var downbeats []int
	for i, b := range beats {
		if floorMod(i-meter.Phase, meter.BeatsPerBar) == 0 {
			downbeats = append(downbeats, b)
		}
	}
	harmony := DetectKey(a.chroma)
	energy := a.energy()
	msPerFeature := int(math.Round(framesPerFeature * 1000 / frameRate))
	sections := Segment(energy, beats, durationMs, a.features, msPerFeature)
	vocals := a.vocals()
	var boundaries []int
	for i, s := range sections {
		if i > 0 {
			boundaries = append(boundaries, s.StartMs)
		}
	}
	phrases := DetectPhrases(downbeats, boundaries)
	var tempoMap []TempoPoint
	if tempo.BPM > 0 {
		tempoMap = []TempoPoint{{TimeMs: 0, BPM: tempo.BPM}}
	}
	a.finished = &Fingerprint{
		Source:     a.source,
		Identifier: a.identifier,
		DurationMs: durationMs,
		Rhythm: Rhythm{
			BPM: tempo.BPM, BPMConfidence: tempo.Confidence, Meter: meter.BeatsPerBar,
			BeatsMs: nonNil(beats), DownbeatsMs: nonNil(downbeats), TempoMap: tempoMap,
			PhrasesMs: nonNil(phrases.PhrasesMs), PhraseBars: phrases.Bars,
		},
		Harmony:         harmony,
		Sections:        sections,
		Energy:          energy,
		Onsets:          a.onsets(),
		Vocals:          vocals,
		MixRegions:      DetectMixRegions(energy, vocals, phrases.PhrasesMs, durationMs),
		Loudness:        LoudnessResult{IntegratedLufs: a.loud.Integrated(), PeakAmplitude: a.loud.Peak()},
		Vector:          a.vector(tempo),
		AnalysisVersion: AnalysisVersion,
		AnalyzerVersion: AnalyzerVersion,
	}
	return a.finished
}

func nonNil(v []int) []int {
	if v == nil {
		return []int{}
	}
	return v
}

func (a *Analyzer) beats(t Tempo, durationMs int) []int {
	if t.PeriodFrames <= 0 {
		return nil
	}
	var out []int
	for pos := t.PhaseFrames; len(out) <= maxBeats; pos += t.PeriodFrames {
		ms := toMs(pos)
		if ms > durationMs {
			break
		}
		out = append(out, ms)
	}
	return out
}

func (a *Analyzer) energy() []EnergyPoint {
	if len(a.rms) == 0 {
		return nil
	}
	per := max(1, int(frameRate/2))
	peak := math.Max(maxOf(a.rms), 1e-6)
	var out []EnergyPoint
	for i := 0; i < len(a.rms); i += per {
		end := min(len(a.rms), i+per)
		out = append(out, EnergyPoint{TimeMs: toMs(float64(i)), Value: clamp(mean(a.rms[i:end])/peak, 0, 1)})
	}
	return out
}

func (a *Analyzer) onsets() []Onset {
	if len(a.flux) == 0 {
		return nil
	}
	threshold := mean(a.flux) * 1.5
	peak := math.Max(maxOf(a.flux), 1e-6)
	var out []Onset
	for i := 1; i < len(a.flux)-1; i++ {
		v := a.flux[i]
		if v > threshold && v >= a.flux[i-1] && v > a.flux[i+1] {
			out = append(out, Onset{TimeMs: toMs(float64(i)), Strength: clamp(v/peak, 0, 1)})
		}
	}
	return out
}

func (a *Analyzer) vocals() []VocalActivity {
	if len(a.vocal) == 0 {
		return nil
	}
	threshold := math.Max(minVocalRatio, mean(a.vocal)*1.1)
	var spans []VocalActivity
	start, acc, count := -1, 0.0, 0
	flush := func(to int) {
		if count == 0 {
			return
		}
		s, e := toMs(float64(start)), toMs(float64(to))
		if e-s < minVocalMs {
			return
		}
		cont := Continuity(a.f0, start, to)
		vib := VibratoStrength(a.f0, frameRate, start, to)
		if cont < minContinuity && vib < minVibrato {
			return
		}
		band := clamp(acc/float64(count), 0, 1)
		spans = append(spans, VocalActivity{StartMs: s, EndMs: e, Confidence: clamp(band*0.5+cont*0.35+vib*0.15, 0, 1)})
	}
	for i, v := range a.vocal {
		active := v >= threshold
		if active && start < 0 {
			start, acc, count = i, 0, 0
		}
		if active {
			acc += v
			count++
		}
		if !active && start >= 0 {
			flush(i)
			start = -1
		}
	}
	if start >= 0 {
		flush(len(a.vocal))
	}
	return spans
}

func (a *Analyzer) vector(t Tempo) []float64 {
	total := 0.0
	for _, c := range a.chroma {
		total += c
	}
	total = math.Max(total, 1e-6)
	v := make([]float64, 0, 16)
	for _, c := range a.chroma {
		v = append(v, c/total)
	}
	v = append(v, clamp(t.BPM/200, 0, 1))
	v = append(v, clamp(mean(a.rms), 0, 1))
	v = append(v, clamp(mean(a.centroid)/8000, 0, 1))
	v = append(v, clamp(mean(a.vocal), 0, 1))
	return v
}
