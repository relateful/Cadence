package analysis

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/greed/cadence/internal/audio"
	"github.com/greed/cadence/internal/source"
)

func TestFFTFindsTone(t *testing.T) {
	n := 1024
	re, im := make([]float64, n), make([]float64, n)
	for i := range re {
		re[i] = math.Sin(2 * math.Pi * 32 * float64(i) / float64(n))
	}
	if err := FFT(re, im); err != nil {
		t.Fatal(err)
	}
	best := 0
	for b := 1; b < n/2; b++ {
		if math.Hypot(re[b], im[b]) > math.Hypot(re[best], im[best]) {
			best = b
		}
	}
	if best != 32 {
		t.Fatalf("peak bin %d", best)
	}
	if FFT(make([]float64, 3), make([]float64, 3)) == nil {
		t.Fatal("non power of two accepted")
	}
}

func TestTempoFromPulseTrain(t *testing.T) {
	rate := 93.75
	env := make([]float64, int(rate*30))
	period := 60 * rate / 128
	for p := 0.0; p < float64(len(env)); p += period {
		env[int(p)] = 1
	}
	tempo := EstimateTempo(env, rate)
	if math.Abs(tempo.BPM-128) > 2 {
		t.Fatalf("bpm %.2f", tempo.BPM)
	}
}

func TestKeyFromCMajorChroma(t *testing.T) {
	var chroma [12]float64
	for _, pc := range []int{0, 4, 7, 0, 2, 5, 9, 11, 0, 7} {
		chroma[pc]++
	}
	h := DetectKey(chroma)
	if h.Key != "C" || h.Scale != "major" || h.Camelot != "8B" {
		t.Fatalf("%+v", h)
	}
	if DetectKey([12]float64{}).Camelot != "8A" {
		t.Fatal("empty chroma fallback")
	}
}

func TestMeterFindsBarPhase(t *testing.T) {
	accents := make([]float64, 64)
	for i := range accents {
		accents[i] = 0.3
		if (i-1)%4 == 0 {
			accents[i] = 1
		}
	}
	m := DetectMeter(accents)
	if m.BeatsPerBar != 4 || m.Phase != 1 {
		t.Fatalf("%+v", m)
	}
	w := make([]float64, 60)
	for i := range w {
		w[i] = 0.2
		if i%3 == 2 {
			w[i] = 1
		}
	}
	if m := DetectMeter(w); m.BeatsPerBar != 3 || m.Phase != 2 {
		t.Fatalf("waltz %+v", m)
	}
}

func TestPhraseGridPicksSixteenBars(t *testing.T) {
	downbeats := make([]int, 128)
	for i := range downbeats {
		downbeats[i] = i * 2000
	}
	p := DetectPhrases(downbeats, []int{4 * 2000, 20 * 2000, 36 * 2000, 52 * 2000})
	if p.Bars != 16 || p.PhaseBar != 4 || p.PhrasesMs[0] != 8000 {
		t.Fatalf("%+v", p)
	}
}

func loudFrames(amplitude float64, seconds int) []audio.Frame {
	frames := make([]audio.Frame, seconds*audio.FramesPerSecond)
	n := 0
	for f := range frames {
		for i := 0; i < audio.FrameSamplesPerChannel; i++ {
			v := audio.Clip(amplitude * 32767 * math.Sin(2*math.Pi*1000*float64(n)/audio.SampleRate))
			frames[f][2*i], frames[f][2*i+1] = v, v
			n++
		}
	}
	return frames
}

func TestLoudnessSixDecibelStep(t *testing.T) {
	measure := func(amp float64) float64 {
		l := NewLoudness()
		for _, f := range loudFrames(amp, 3) {
			l.Feed(&f)
		}
		return l.Integrated()
	}
	diff := measure(0.5) - measure(0.25)
	if math.Abs(diff-6.02) > 0.1 {
		t.Fatalf("step %.3f dB", diff)
	}
	if NewLoudness().Integrated() != FloorLUFS {
		t.Fatal("empty loudness")
	}
}

func TestAnalyzerProducesCompleteFingerprint(t *testing.T) {
	var got *Fingerprint
	a := NewAnalyzer("test", "tone", func(f *Fingerprint) { got = f })
	for _, f := range loudFrames(0.4, 20) {
		a.Feed(&f)
	}
	a.Finish()
	a.Finish()
	if got == nil {
		t.Fatal("no fingerprint")
	}
	if got.DurationMs != 20000 || got.AnalysisVersion != AnalysisVersion || len(got.Vector) != 16 || len(got.Energy) == 0 {
		t.Fatalf("fingerprint %+v", got)
	}
	if got.Rhythm.BeatsMs == nil || got.Rhythm.DownbeatsMs == nil || got.Rhythm.PhrasesMs == nil {
		t.Fatal("rhythm arrays must serialize as [] not null")
	}
}

func TestRegistryIsBounded(t *testing.T) {
	r := NewRegistry()
	for i := 0; i < registrySize+10; i++ {
		r.Store(&Fingerprint{Source: "s", Identifier: string(rune('a'+i%26)) + string(rune(i))})
	}
	if len(r.items) != registrySize {
		t.Fatalf("%d entries", len(r.items))
	}
}

func TestOfflineQueueDropsWhenFull(t *testing.T) {
	block := make(chan struct{})
	open := func(ctx context.Context, _ source.Track) (source.Stream, error) {
		<-block
		return source.Stream{}, errors.New("closed")
	}
	o := NewOffline(NewRegistry(), open, 1, 2, discard())
	accepted := 0
	for i := 0; i < 10; i++ {
		if o.Enqueue(source.Track{Source: "s", Identifier: string(rune('a' + i))}, false) {
			accepted++
		}
	}
	if accepted > 3 || o.Stats().Dropped < 7 {
		t.Fatalf("accepted %d dropped %d", accepted, o.Stats().Dropped)
	}
	if o.Enqueue(source.Track{Source: "s", Identifier: "a"}, false) {
		t.Fatal("in-flight track queued twice")
	}
	close(block)
	deadline := time.Now().Add(2 * time.Second)
	for o.Stats().Failed < int64(accepted) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if o.Enqueue(source.Track{Source: "s", Identifier: "a"}, false) {
		t.Fatal("a track that just failed was queued again before failRetry")
	}
	if o.Enqueue(source.Track{Source: "s", Identifier: "a"}, true) {
		t.Fatal("force bypassed the failure memory")
	}
	o.mu.Lock()
	o.failures["s:a"] = time.Now().Add(-failRetry)
	o.mu.Unlock()
	if !o.Enqueue(source.Track{Source: "s", Identifier: "a"}, false) {
		t.Fatal("finished track was not released after failRetry")
	}
}
