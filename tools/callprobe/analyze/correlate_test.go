package analyze

import (
	"math"
	"math/rand/v2"
	"testing"
)

// The cross-correlation runs in FFT blocks; each lag's value is the direct
// sum over every sample where both files have one. A block boundary off by
// one, a negative lag counted from the wrong end, or a missing zero past a
// file's end changes some lag's sum.
func TestCorrelateMatchesTheDirectSum(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	signal := func(n int) []float32 {
		x := make([]float32, n)
		for i := range x {
			x[i] = float32(r.NormFloat64())
		}
		return x
	}
	for _, tc := range []struct {
		name           string
		nx, ny, lo, hi int
		size           int
	}{
		{"both signs of lag, many blocks", 3000, 2600, -150, 220, 1024},
		{"the reference longer than the recording", 2600, 700, -400, 90, 1024},
		{"positive lags only, as an autocorrelation", 3000, 3000, 0, 40, 256},
		{"one block", 500, 500, -20, 20, 4096},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x, y := signal(tc.nx), signal(tc.ny)
			got := correlateSize(x, y, tc.lo, tc.hi, tc.size)
			if len(got) != tc.hi-tc.lo+1 {
				t.Fatalf("got %d lags, want %d", len(got), tc.hi-tc.lo+1)
			}
			for i := range got {
				lag := tc.lo + i
				var want float64
				for n := range x {
					if m := n + lag; m >= 0 && m < len(y) {
						want += float64(x[n]) * float64(y[m])
					}
				}
				if !within(got[i], want, 1e-6) {
					t.Fatalf("lag %d: got %v, want %v", lag, got[i], want)
				}
			}
		})
	}
}

// filter puts a reference through an echo path in FFT blocks; each output
// sample is the direct sum over the path's taps where the reference has a
// sample. A block's tail added to the wrong sample, a path's first lag
// counted from the wrong end, or an output past n changes some sample.
func TestFilterMatchesTheDirectSum(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	for _, tc := range []struct {
		name               string
		nx, taps, first, n int
		size               int
	}{
		{"a path after the start, many blocks", 3000, 40, 150, 3300, 256},
		{"a path before the start", 3000, 40, -120, 2900, 512},
		{"an output shorter than the path's reach", 3000, 40, 10, 1000, 1024},
		{"one block", 500, 20, -5, 600, 4096},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := make([]float32, tc.nx)
			for i := range x {
				x[i] = float32(r.NormFloat64())
			}
			h := make([]float64, tc.taps)
			for j := range h {
				h[j] = r.NormFloat64()
			}
			got := filterSize(x, h, tc.first, tc.n, tc.size)
			if len(got) != tc.n {
				t.Fatalf("got %d samples, want %d", len(got), tc.n)
			}
			for m := range got {
				var want float64
				for j, v := range h {
					if i := m - tc.first - j; i >= 0 && i < len(x) {
						want += v * float64(x[i])
					}
				}
				if !within(float64(got[m]), want, 1e-4) {
					t.Fatalf("sample %d: got %v, want %v", m, got[m], want)
				}
			}
		})
	}
}

// within says got is want within tolerance; NaN is within nothing.
func within(got, want, tolerance float64) bool { return math.Abs(got-want) <= tolerance }

// TA7: |NaN - want| > tolerance is false, so a check written that way passes
// a NaN. within takes no NaN and no infinity, and takes a finite value
// within its tolerance.
func TestWithinTakesNoNaN(t *testing.T) {
	for _, got := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if within(got, 0, 1e-6) {
			t.Errorf("within(%v, 0, 1e-6) = true, want false", got)
		}
	}
	if !within(1+1e-7, 1, 1e-6) {
		t.Error("within(1+1e-7, 1, 1e-6) = false, want true")
	}
}
