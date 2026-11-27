package analyze

import (
	"math/rand/v2"
	"testing"
)

// The second copy's fit needs each entry of its normal equations summed
// over exactly the samples both paths reach; gram builds them from a
// cross-correlation per block and one sample's step along each diagonal. A
// step that drops a sample, a block filled from the wrong row, or a mirrored
// entry left out changes some entry from the direct sum.
func TestGramMatchesTheDirectSum(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 6))
	for _, tc := range []struct {
		name                        string
		nx, ny, first, second, taps int
	}{
		{"both paths after the start", 3000, 3300, 40, 130, 24},
		{"the first path before the start", 3000, 2900, -60, 20, 24},
		{"paths that touch", 2000, 2000, -10, 6, 16},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := make([]float32, tc.nx)
			for i := range x {
				x[i] = float32(r.NormFloat64())
			}
			m0, m1 := tc.second+tc.taps-1, min(tc.ny, tc.nx+tc.first)
			g := gram(x, m0, m1, tc.first, tc.second, tc.taps)
			n := 2 * tc.taps
			lag := func(p int) int {
				if p < tc.taps {
					return tc.first + p
				}
				return tc.second + p - tc.taps
			}
			for p := range n {
				for q := range n {
					var want float64
					for m := m0; m < m1; m++ {
						want += float64(x[m-lag(p)]) * float64(x[m-lag(q)])
					}
					if !within(g[p*n+q], want, 1e-6) {
						t.Fatalf("lags %d and %d: got %v, want %v", lag(p), lag(q), g[p*n+q], want)
					}
				}
			}
		})
	}
}
