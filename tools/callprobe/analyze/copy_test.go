package analyze

import (
	"math/rand/v2"
	"testing"
)

// The second copy's fit needs each entry of its normal equations summed
// over exactly the samples of its window, the reference taken as 0 outside
// its own; gram builds them from a cross-correlation per block and one
// sample's step along each diagonal. A step that drops a sample, a block
// filled from the wrong row, a mirrored entry left out, or a sample read
// past either end of the reference changes some entry from the direct sum.
// The window either holds the reference at every lag of both paths, or is
// the fit's: from the first path's first lag to the second path's reach
// past the reference's end.
func TestGramMatchesTheDirectSum(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 6))
	for _, tc := range []struct {
		name                        string
		nx, ny, first, second, taps int
		fit                         bool // the fit's window, past the reference's ends
	}{
		{"both paths after the start", 3000, 3300, 40, 130, 24, false},
		{"the first path before the start", 3000, 2900, -60, 20, 24, false},
		{"paths that touch", 2000, 2000, -10, 6, 16, false},
		{"the fit's window past both ends", 3000, 3400, 40, 130, 24, true},
		{"the fit's window with the first path before the start", 3000, 3300, -60, 20, 24, true},
		{"the fit's window cut by the recording's end", 3000, 3100, 40, 130, 24, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := make([]float32, tc.nx)
			for i := range x {
				x[i] = float32(r.NormFloat64())
			}
			m0, m1 := tc.second+tc.taps-1, min(tc.ny, tc.nx+tc.first)
			if tc.fit {
				m0, m1 = max(0, tc.first), min(tc.ny, tc.nx+tc.second+tc.taps-1)
			}
			g := gram(x, m0, m1, tc.first, tc.second, tc.taps)
			n := 2 * tc.taps
			lag := func(p int) int {
				if p < tc.taps {
					return tc.first + p
				}
				return tc.second + p - tc.taps
			}
			sample := func(i int) float64 {
				if i < 0 || i >= len(x) {
					return 0
				}
				return float64(x[i])
			}
			for p := range n {
				for q := range n {
					var want float64
					for m := m0; m < m1; m++ {
						want += sample(m-lag(p)) * sample(m-lag(q))
					}
					if !within(g[p*n+q], want, 1e-6) {
						t.Fatalf("lags %d and %d: got %v, want %v", lag(p), lag(q), g[p*n+q], want)
					}
				}
			}
		})
	}
}
