package audio

import "math"

const (
	sincZeroCrossings = 16
	sincPhases        = 256
	kaiserBeta        = 8.6
)

type kernel struct {
	cutoff float64
	table  [][]float64
}

func bessel0(x float64) float64 {
	sum, term := 1.0, 1.0
	for k := 1; k < 32; k++ {
		term *= (x / (2 * float64(k))) * (x / (2 * float64(k)))
		sum += term
	}
	return sum
}

func newKernel(cutoff float64) *kernel {
	taps := 2 * sincZeroCrossings
	table := make([][]float64, sincPhases+1)
	norm := bessel0(kaiserBeta)
	for p := 0; p <= sincPhases; p++ {
		frac := float64(p) / sincPhases
		row := make([]float64, taps)
		for t := 0; t < taps; t++ {
			x := float64(t-sincZeroCrossings+1) - frac
			arg := x * cutoff
			s := cutoff
			if arg != 0 {
				s = math.Sin(math.Pi*arg) / (math.Pi * arg) * cutoff
			}
			w := x / sincZeroCrossings
			win := 0.0
			if math.Abs(w) <= 1 {
				win = bessel0(kaiserBeta*math.Sqrt(1-w*w)) / norm
			}
			row[t] = s * win
		}
		table[p] = row
	}
	return &kernel{cutoff: cutoff, table: table}
}

type Resampler struct {
	step   float64
	kern   *kernel
	left   []float64
	right  []float64
	cursor float64
	base   int64
}

func NewResampler(inRate int, factor float64) *Resampler {
	r := &Resampler{}
	r.Configure(inRate, factor)
	return r
}

func (r *Resampler) Configure(inRate int, factor float64) {
	step := float64(inRate) * factor / SampleRate
	if r.kern != nil && math.Abs(step-r.step) < 1e-9 {
		return
	}
	r.step = step
	cutoff := 1.0
	if step > 1 {
		cutoff = 1 / step
	}
	r.kern = newKernel(cutoff * 0.97)
}

func (r *Resampler) Step() float64 {
	return r.step
}

func (r *Resampler) Write(interleaved []int16) {
	for i := 0; i+1 < len(interleaved); i += 2 {
		r.left = append(r.left, float64(interleaved[i]))
		r.right = append(r.right, float64(interleaved[i+1]))
	}
}

func (r *Resampler) Position() float64 {
	return float64(r.base) + r.cursor
}

func (r *Resampler) Available() int {
	usable := float64(len(r.left)-sincZeroCrossings) - r.cursor
	if usable <= 0 {
		return 0
	}
	return int(usable / r.step)
}

func (r *Resampler) Read(out []float64) int {
	frames := len(out) / 2
	n := 0
	for n < frames {
		idx := int(r.cursor)
		if idx+sincZeroCrossings >= len(r.left) {
			break
		}
		frac := r.cursor - float64(idx)
		row := r.kern.table[int(frac*sincPhases+0.5)]
		var l, rr float64
		start := idx - sincZeroCrossings + 1
		for t, w := range row {
			j := start + t
			if j < 0 {
				continue
			}
			l += r.left[j] * w
			rr += r.right[j] * w
		}
		out[2*n], out[2*n+1] = l, rr
		r.cursor += r.step
		n++
	}
	r.compact()
	return n
}

func (r *Resampler) compact() {
	drop := int(r.cursor) - sincZeroCrossings
	if drop < 4096 {
		return
	}
	r.left = append(r.left[:0], r.left[drop:]...)
	r.right = append(r.right[:0], r.right[drop:]...)
	r.cursor -= float64(drop)
	r.base += int64(drop)
}

func (r *Resampler) Reset(base int64) {
	r.left, r.right = r.left[:0], r.right[:0]
	r.cursor = 0
	r.base = base
}
