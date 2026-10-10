package analysis

import (
	"container/list"
	"sync"
)

const (
	AnalysisVersion = 3
	AnalyzerVersion = "cadence/1"
	registrySize    = 512
)

type TempoPoint struct {
	TimeMs int     `json:"timeMs"`
	BPM    float64 `json:"bpm"`
}

type Rhythm struct {
	BPM           float64      `json:"bpm"`
	BPMConfidence float64      `json:"bpmConfidence"`
	Meter         int          `json:"meter"`
	BeatsMs       []int        `json:"beatsMs"`
	DownbeatsMs   []int        `json:"downbeatsMs"`
	TempoMap      []TempoPoint `json:"tempoMap"`
	PhrasesMs     []int        `json:"phrasesMs"`
	PhraseBars    int          `json:"phraseBars"`
}

type Harmony struct {
	Camelot    string  `json:"camelot"`
	Key        string  `json:"key"`
	Scale      string  `json:"scale"`
	Confidence float64 `json:"confidence"`
}

type Section struct {
	Type      string  `json:"type"`
	StartMs   int     `json:"startMs"`
	EndMs     int     `json:"endMs"`
	Energy    float64 `json:"energy"`
	BeatStart int     `json:"beatStart"`
	BeatEnd   int     `json:"beatEnd"`
}

type MixRegions struct {
	IntroStartMs int `json:"introStartMs"`
	IntroEndMs   int `json:"introEndMs"`
	OutroStartMs int `json:"outroStartMs"`
	OutroEndMs   int `json:"outroEndMs"`
}

type LoudnessResult struct {
	IntegratedLufs float64 `json:"integratedLufs"`
	PeakAmplitude  float64 `json:"peakAmplitude"`
}

type EnergyPoint struct {
	TimeMs int     `json:"timeMs"`
	Value  float64 `json:"value"`
}

type Onset struct {
	TimeMs   int     `json:"timeMs"`
	Strength float64 `json:"strength"`
}

type VocalActivity struct {
	StartMs    int     `json:"startMs"`
	EndMs      int     `json:"endMs"`
	Confidence float64 `json:"confidence"`
}

type Fingerprint struct {
	Source          string          `json:"source"`
	Identifier      string          `json:"identifier"`
	DurationMs      int             `json:"durationMs"`
	Rhythm          Rhythm          `json:"rhythm"`
	Harmony         Harmony         `json:"harmony"`
	Sections        []Section       `json:"sections"`
	Energy          []EnergyPoint   `json:"energy"`
	Onsets          []Onset         `json:"onsets"`
	Vocals          []VocalActivity `json:"vocals"`
	MixRegions      MixRegions      `json:"mixRegions"`
	Loudness        LoudnessResult  `json:"loudness"`
	Vector          []float64       `json:"vector"`
	AnalysisVersion int             `json:"analysisVersion"`
	AnalyzerVersion string          `json:"analyzerVersion"`
}

type Registry struct {
	mu    sync.Mutex
	order *list.List
	items map[string]*list.Element
}

func NewRegistry() *Registry {
	return &Registry{order: list.New(), items: map[string]*list.Element{}}
}

func key(source, identifier string) string {
	return source + ":" + identifier
}

func (r *Registry) Store(f *Fingerprint) {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := key(f.Source, f.Identifier)
	if el, ok := r.items[k]; ok {
		el.Value = f
		r.order.MoveToBack(el)
		return
	}
	r.items[k] = r.order.PushBack(f)
	for r.order.Len() > registrySize {
		front := r.order.Front()
		r.order.Remove(front)
		old := front.Value.(*Fingerprint)
		delete(r.items, key(old.Source, old.Identifier))
	}
}

func (r *Registry) Get(source, identifier string) (*Fingerprint, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	el, ok := r.items[key(source, identifier)]
	if !ok {
		return nil, false
	}
	return el.Value.(*Fingerprint), true
}

func (r *Registry) Has(source, identifier string) bool {
	_, ok := r.Get(source, identifier)
	return ok
}
