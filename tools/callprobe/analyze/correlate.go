package analyze

import (
	"math"
	"math/bits"
	"math/cmplx"
)

// fft is a radix-2 Fourier transform of one size, made once and used on each
// block.
type fft struct {
	twiddle []complex128 // exp(-2πik/n), k < n/2
	reverse []int32      // each index's bits reversed
}

func newFFT(n int) *fft {
	f := &fft{twiddle: make([]complex128, n/2), reverse: make([]int32, n)}
	for k := range f.twiddle {
		s, c := math.Sincos(-2 * math.Pi * float64(k) / float64(n))
		f.twiddle[k] = complex(c, s)
	}
	shift := bits.UintSize - bits.TrailingZeros(uint(n))
	for i := range f.reverse {
		f.reverse[i] = int32(bits.Reverse(uint(i)) >> shift)
	}
	return f
}

// transform replaces a, of the fft's size, with its transform, or with its
// inverse scaled by 1/n.
func (f *fft) transform(a []complex128, inverse bool) {
	n := len(a)
	for i, j := range f.reverse {
		if int32(i) < j {
			a[i], a[j] = a[j], a[i]
		}
	}
	for size := 2; size <= n; size <<= 1 {
		half, step := size/2, n/size
		for start := 0; start < n; start += size {
			for k := range half {
				w := f.twiddle[k*step]
				if inverse {
					w = cmplx.Conj(w)
				}
				u, v := a[start+k], a[start+k+half]*w
				a[start+k], a[start+k+half] = u+v, u-v
			}
		}
	}
	if inverse {
		scale := complex(1/float64(n), 0)
		for i := range a {
			a[i] *= scale
		}
	}
}

// correlate is, for each lag from lo to hi, the sum of x[n]·y[n+lag] over
// every n where both have a sample.
func correlate(x, y []float32, lo, hi int) []float64 {
	lags := hi - lo + 1
	block := min(len(x), max(3*lags, 1<<14))
	return correlateSize(x, y, lo, hi, 1<<bits.Len(uint(block+lags-2)))
}

// correlateSize is correlate in FFT blocks of size, which must exceed the
// number of lags. Each block of x is transformed with the stretch of y its
// lags reach, packed as one complex signal (x real, y imaginary), so a block
// takes two transforms, not three.
func correlateSize(x, y []float32, lo, hi, size int) []float64 {
	lags := hi - lo + 1
	block := size - lags + 1
	out := make([]float64, lags)
	f := newFFT(size)
	z := make([]complex128, size)
	c := make([]complex128, size)
	for start := 0; start < len(x); start += block {
		end := min(start+block, len(x))
		clear(z)
		for n := start; n < end; n++ {
			z[n-start] = complex(float64(x[n]), 0)
		}
		// y from start+lo, as far as the block's last sample reaches at lag
		// hi; zero where y has no sample.
		from, to := max(0, start+lo), min(len(y), end-1+hi+1)
		for m := from; m < to; m++ {
			z[m-start-lo] += complex(0, float64(y[m]))
		}
		f.transform(z, false)
		for k := range size {
			zk, zn := z[k], cmplx.Conj(z[(size-k)%size])
			xk, yk := (zk+zn)/2, (zk-zn)/complex(0, 2)
			c[k] = cmplx.Conj(xk) * yk
		}
		f.transform(c, true)
		for k := range out {
			out[k] += real(c[k])
		}
	}
	return out
}

// filter is x through the echo path h over the lags first to
// first+len(h)-1: for each m below n, the sum of h[j]·x[m-first-j] over
// every j where x has that sample.
func filter(x []float32, h []float64, first, n int) []float32 {
	return filterSize(x, h, first, n, 1<<bits.Len(uint(max(4*len(h), 1<<14)-1)))
}

// filterSize is filter in FFT blocks of size, which must exceed len(h)-1.
// Each block of x is transformed, multiplied by the path's transform, and
// added back at its place, the path's tail running on into the next block's.
func filterSize(x []float32, h []float64, first, n, size int) []float32 {
	out := make([]float32, n)
	block := size - len(h) + 1
	f := newFFT(size)
	path := make([]complex128, size)
	for j, v := range h {
		path[j] = complex(v, 0)
	}
	f.transform(path, false)
	z := make([]complex128, size)
	for start := 0; start < len(x) && start+first < n; start += block {
		end := min(start+block, len(x))
		if end+first+len(h) < 2 { // each sample the block gives is before out's first
			continue
		}
		clear(z)
		for i := start; i < end; i++ {
			z[i-start] = complex(float64(x[i]), 0)
		}
		f.transform(z, false)
		for k := range z {
			z[k] *= path[k]
		}
		f.transform(z, true)
		for i, v := range z[:end-start+len(h)-1] {
			if m := start + first + i; m >= 0 && m < n {
				out[m] += float32(real(v))
			}
		}
	}
	return out
}
