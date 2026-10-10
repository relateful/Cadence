package audio

import "math"

const (
	stretchHop    = FrameSamplesPerChannel
	stretchWindow = stretchHop * 2
	stretchSearch = 480
	searchStride  = 4
)

var hann = func() []float64 {
	w := make([]float64, stretchWindow)
	for i := range w {
		w[i] = 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(stretchWindow))
	}
	return w
}()

type Stretcher struct {
	tempo     float64
	in        []float64
	pos       []float64
	nominal   float64
	prevStart int
	tail      []float64
	started   bool
}

func NewStretcher() *Stretcher {
	return &Stretcher{tempo: 1, tail: make([]float64, stretchHop*2)}
}

func (s *Stretcher) SetTempo(tempo float64) {
	if tempo <= 0 {
		tempo = 1
	}
	s.tempo = math.Min(4, math.Max(0.25, tempo))
}

func (s *Stretcher) Tempo() float64 {
	return s.tempo
}

func (s *Stretcher) frames() int {
	return len(s.pos)
}

func (s *Stretcher) Write(samples []float64, positions []float64) {
	s.in = append(s.in, samples...)
	s.pos = append(s.pos, positions...)
}

func (s *Stretcher) Buffered() int {
	return s.frames() - int(s.nominal)
}

func (s *Stretcher) Reset() {
	s.in, s.pos = s.in[:0], s.pos[:0]
	s.nominal, s.prevStart, s.started = 0, 0, false
	clear(s.tail)
}

func (s *Stretcher) Next(out []float64) (float64, bool) {
	if s.tempo == 1 && !s.started {
		return s.passthrough(out)
	}
	start := int(s.nominal)
	if start+stretchSearch+stretchWindow >= s.frames() {
		return 0, false
	}
	if s.started {
		start = s.bestOffset(start)
	} else {
		s.started = true
	}
	for i := 0; i < stretchHop; i++ {
		w1, w2 := hann[i], hann[i+stretchHop]
		for c := 0; c < 2; c++ {
			out[2*i+c] = s.tail[2*i+c] + s.in[2*(start+i)+c]*w1
			s.tail[2*i+c] = s.in[2*(start+stretchHop+i)+c] * w2
		}
	}
	position := s.pos[start]
	s.prevStart = start
	s.nominal += stretchHop * s.tempo
	s.compact()
	return position, true
}

func (s *Stretcher) passthrough(out []float64) (float64, bool) {
	start := int(s.nominal)
	if start+stretchHop > s.frames() {
		return 0, false
	}
	copy(out, s.in[2*start:2*(start+stretchHop)])
	position := s.pos[start]
	s.prevStart = start
	s.nominal += stretchHop
	s.compact()
	return position, true
}

func (s *Stretcher) bestOffset(nominal int) int {
	natural := s.prevStart + stretchHop
	best, bestScore := nominal, math.Inf(-1)
	lo := max(0, nominal-stretchSearch)
	for cand := lo; cand <= nominal+stretchSearch; cand += searchStride {
		score := 0.0
		for i := 0; i < stretchHop; i += searchStride {
			a := s.in[2*(natural+i)] + s.in[2*(natural+i)+1]
			b := s.in[2*(cand+i)] + s.in[2*(cand+i)+1]
			score += a * b
		}
		if score > bestScore {
			best, bestScore = cand, score
		}
	}
	return best
}

func (s *Stretcher) compact() {
	keep := min(int(s.nominal), s.prevStart) - stretchSearch
	if keep < 8192 {
		return
	}
	s.in = append(s.in[:0], s.in[2*keep:]...)
	s.pos = append(s.pos[:0], s.pos[keep:]...)
	s.nominal -= float64(keep)
	s.prevStart -= keep
}
