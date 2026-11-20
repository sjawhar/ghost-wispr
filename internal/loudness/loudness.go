// Package loudness measures audio as ITU-R BS.1770-4 does: loudness in LUFS
// and true peak in dBTP. The tests of Wispr's sounds and of the speaker's
// mixer use it.
package loudness

import (
	"encoding/binary"
	"math"
)

// Samples reads PCM16 LE as samples of full scale 1.
func Samples(pcm []byte) []float64 {
	x := make([]float64, len(pcm)/2)
	for i := range x {
		x[i] = float64(int16(binary.LittleEndian.Uint16(pcm[2*i:]))) / 32768
	}
	return x
}

// Integrated is x's BS.1770-4 integrated loudness in LUFS, over its own
// length: the gated mean power of its K-weighted 400 ms blocks, 100 ms apart,
// or of one block of all of x when x is shorter. Blocks under -70 LUFS are
// dropped, then blocks 10 LU under the mean of the rest; it is -Inf when no
// block is left, or x is empty.
func Integrated(x []float64, rate int) float64 {
	z := kWeighted(x, float64(rate))
	block, hop := min(len(z), rate*4/10), rate/10
	if block == 0 {
		return math.Inf(-1)
	}
	var powers []float64
	for start := 0; start+block <= len(z); start += hop {
		var sum float64
		for _, v := range z[start : start+block] {
			sum += v * v
		}
		powers = append(powers, sum/float64(block))
	}
	lufs := func(p float64) float64 { return -0.691 + 10*math.Log10(p) }
	gated := func(gate float64) float64 {
		var sum float64
		var n int
		for _, p := range powers {
			if lufs(p) > gate {
				sum += p
				n++
			}
		}
		if n == 0 {
			return math.Inf(-1)
		}
		return lufs(sum / float64(n))
	}
	absolute := gated(-70)
	return gated(max(-70, absolute-10))
}

// kWeighted filters x, sampled at rate, through BS.1770's K-weighting: its
// high shelf, then its high-pass, with the coefficients libebur128 derives
// for rate.
func kWeighted(x []float64, rate float64) []float64 {
	k := math.Tan(math.Pi * 1681.974450955533 / rate)
	q := 0.7071752369554196
	vh := math.Pow(10, 3.999843853973347/20)
	vb := math.Pow(vh, 0.4996667741545416)
	a0 := 1 + k/q + k*k
	shelf := biquad{
		b: [3]float64{(vh + vb*k/q + k*k) / a0, 2 * (k*k - vh) / a0, (vh - vb*k/q + k*k) / a0},
		a: [2]float64{2 * (k*k - 1) / a0, (1 - k/q + k*k) / a0},
	}
	k = math.Tan(math.Pi * 38.13547087602444 / rate)
	q = 0.5003270373238773
	a0 = 1 + k/q + k*k
	highPass := biquad{b: [3]float64{1, -2, 1}, a: [2]float64{2 * (k*k - 1) / a0, (1 - k/q + k*k) / a0}}
	return highPass.filter(shelf.filter(x))
}

// biquad is a second-order filter normalized so that a0 is 1: b holds b0, b1
// and b2, and a holds a1 and a2. filter runs it in direct form I from rest.
type biquad struct {
	b [3]float64
	a [2]float64
}

func (f biquad) filter(x []float64) []float64 {
	y := make([]float64, len(x))
	var x1, x2, y1, y2 float64
	for i, v := range x {
		y[i] = f.b[0]*v + f.b[1]*x1 + f.b[2]*x2 - f.a[0]*y1 - f.a[1]*y2
		x2, x1, y2, y1 = x1, v, y1, y[i]
	}
	return y
}

// TruePeak is x's true peak in dBTP: its highest magnitude with x, sampled at
// rate, oversampled by a Hann-windowed sinc to 192 kHz or more, as BS.1770-4's
// true-peak meter does.
func TruePeak(x []float64, rate int) float64 {
	const half = 32
	over := (192000 + rate - 1) / rate
	var peak float64
	for i := range x {
		peak = max(peak, math.Abs(x[i]))
		for p := 1; p < over; p++ {
			var s float64
			for k := max(-half+1, -i); k <= half && i+k < len(x); k++ {
				d := float64(p)/float64(over) - float64(k)
				s += x[i+k] * math.Sin(math.Pi*d) / (math.Pi * d) * (0.5 + 0.5*math.Cos(math.Pi*d/half))
			}
			peak = max(peak, math.Abs(s))
		}
	}
	return 20 * math.Log10(peak)
}
