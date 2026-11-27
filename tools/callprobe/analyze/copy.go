package analyze

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"time"
)

// CopyOptions are where FindCopy looks for a copy and how much of its path it
// fits.
type CopyOptions struct {
	MaxLag time.Duration // the copy is looked for from MaxLag before the reference to MaxLag after
	Path   time.Duration // the length of the echo path fitted, centred on the strongest lag
	// SecondAfter, when not 0, also looks for a second copy at least this
	// long after the first, the strongest, which is then the direct one: as
	// the copy of Wispr's voice that comes back through the office's
	// microphone comes after the one Ghost Wispr sends. It is at least Path,
	// so that the two paths do not overlap.
	SecondAfter time.Duration
}

// Copy is what FindCopy found of a reference in a recording. Its levels are
// energies against the reference's where the two files overlap, in dB.
type Copy struct {
	Silent  bool          // the recording is digital silence there: nothing can be told
	Found   bool          // a copy stands out of the cross-correlation's noise
	Lag     time.Duration // the recording holds the copy this much later (Found only)
	LevelDB float64       // the copy (Found only): with a Second told, the first path's of the two fitted at once
	FloorDB float64       // the rest of the recording, its noise floor (not Silent): with a Second told, what neither path explains
	Second  *SecondCopy   // with CopyOptions.SecondAfter, when the first copy was found above the floor
}

// SecondCopy is the strongest copy at least CopyOptions.SecondAfter after
// the first, its path fitted together with the first's. When the fit cannot
// tell it from the first, Unknown says why: then no level of it is known,
// and the floor is no bound on it.
type SecondCopy struct {
	Unknown string        // why the fit cannot tell a second copy, or ""
	Found   bool          // a copy stands out of the cross-correlation's noise, the first copy taken out
	Lag     time.Duration // the recording holds it this much later than the reference (Found only)
	LevelDB float64       // against the reference's energy that the recording holds at its lag (Found only)
}

// BelowFloor says a found copy is no louder than the rest of the recording,
// so its level cannot be told from the noise: it is UNKNOWN, never a number.
func (c Copy) BelowFloor() bool { return c.LevelDB <= c.FloorDB }

// SecondBelowFloor says the same of a found second copy.
func (c Copy) SecondBelowFloor() bool { return c.Second.LevelDB <= c.FloorDB }

// foundFactor is how many times the energy that noise alone would fit a
// copy's path must stand above it to be a copy. Noise fits a path of n taps,
// chosen at the strongest of K lags, about n + 2·ln(K) times its energy per
// sample; a factor of 4 is far above where noise reaches.
const foundFactor = 4

// FindCopy cross-correlates rec with ref over every lag within o.MaxLag, and
// at the strongest fits the echo path from ref to rec over o.Path by least
// squares. The copy's level is the energy that path carries, so an echo
// spread over several paths reads as their sum, and the noise floor is the
// energy of rec the path does not explain, which an echo the fit misses
// (one that changes, or is not linear) raises.
func FindCopy(ref, rec Audio, o CopyOptions) (Copy, error) {
	if o.MaxLag < 0 || o.Path <= 0 || o.SecondAfter < 0 || (o.SecondAfter > 0 && o.SecondAfter < o.Path) {
		return Copy{}, errors.New("max-lag must be non-negative, path positive, and second-after 0 or at least the path")
	}
	if ref.Rate != rec.Rate {
		return Copy{}, fmt.Errorf("the reference is at %d Hz and the recording at %d Hz: convert one with ffmpeg -ar", ref.Rate, rec.Rate)
	}
	k, taps := ref.samples(o.MaxLag), max(1, ref.samples(o.Path))
	if 2*k+1 < taps {
		return Copy{}, fmt.Errorf("a lag range of ±%v is shorter than the path of %v", o.MaxLag, o.Path)
	}
	x, y := ref.Samples, rec.Samples
	if energy(y) == 0 {
		return Copy{Silent: true}, nil
	}
	if energy(x) == 0 {
		return Copy{}, errors.New("the reference is digital silence")
	}
	r := correlate(x, y, -k, k)
	peak := 0
	for i, v := range r {
		if math.Abs(v) > math.Abs(r[peak]) {
			peak = i
		}
	}
	lag := peak - k
	first := min(max(lag-taps/2, -k), k-taps+1) // the path's first lag

	// Where the two overlap at the copy's lag.
	from, to := overlap(x, y, lag)
	if to-from < 4*taps {
		return Copy{}, fmt.Errorf("the files overlap by %d samples at a lag of %v: too few to measure", max(0, to-from), ref.At(lag)-ref.Start)
	}
	recEnergy, refEnergy := energy(y[from+lag:to+lag]), energy(x[from:to])
	if recEnergy == 0 {
		return Copy{Silent: true}, nil
	}
	if refEnergy == 0 {
		return Copy{}, errors.New("the reference is digital silence where the files overlap")
	}
	cross := r[first+k : first+k+taps]
	path, err := fitPath(x[from:to], cross)
	if err != nil {
		return Copy{}, err
	}
	var fitted float64
	for i, h := range path {
		fitted += h * cross[i]
	}
	rest := max(recEnergy-fitted, 0)
	c := Copy{FloorDB: 10 * math.Log10(rest/refEnergy)}
	noiseFit := (float64(taps) + 2*math.Log(float64(2*k+1))) / float64(to-from) * rest
	if fitted <= foundFactor*noiseFit {
		return c, nil
	}
	c.Found = true
	c.Lag = ref.At(lag) - ref.Start
	c.LevelDB = 10 * math.Log10(fitted/refEnergy)
	if o.SecondAfter == 0 || c.BelowFloor() {
		return c, nil
	}
	return findSecond(ref, rec, c, k, lag, first, path, ref.samples(o.SecondAfter))
}

// overlap is where x overlaps y when y holds x lag samples later: x[from:to].
func overlap(x, y []float32, lag int) (from, to int) {
	return max(0, -lag), min(len(x), len(y)-lag)
}

// fitPath fits, by least squares, the echo path whose cross-correlations
// with the recording at its lags are cross, against the autocorrelation of
// ref, the reference where it overlaps the recording at the path's lag:
// the normal equations, lightly loaded so that a reference with no energy
// in some band still gives a solution.
func fitPath(ref []float32, cross []float64) ([]float64, error) {
	taps := len(cross)
	auto := correlate(ref, ref, 0, taps-1)
	normal := make([]float64, taps*taps)
	for i := range taps {
		for j := range taps {
			normal[i*taps+j] = auto[max(i-j, j-i)]
		}
		normal[i*taps+i] += 1e-9 * auto[0]
	}
	return solveSPD(normal, cross)
}

// minOwn is the least share of the energy that the reference gives the
// second path's lags, over the fit's samples, that the first path's lags
// must be unable to reproduce for the fit to tell the two copies apart. A
// steady tone delayed is the same tone at another phase, which the first
// path reproduces, and a constant delayed is the same constant: below this
// share, what tells the two paths apart is the small rest, and where the
// files start and end. A steady tone of 30 % of the reference's power
// leaves 70 %; a constant leaves about 2 %.
const minOwn = 0.5

// findSecond finds the second copy for FindCopy: the strongest copy at
// least after samples after the first, found at lag with its path h over
// the lags from first. It takes the first copy out of the recording and
// finds the strongest lag left from lag+after. It then fits both paths at
// once by least squares, so that a reference that correlates with itself
// between the two lags, as a voice's pitch does, still has each copy's
// energy go to its own path. The fit is over every sample of the recording
// that either path reaches with a sample of the reference, the reference
// taken as silence outside its own: from the first copy's start to the
// second's end, past the reference's end, which is where the copy of a
// sound at the reference's end is. The levels and the floor are the
// energies of each path's copy and of what neither explains, over those
// samples, against the reference's energy that the recording holds at
// each copy's lag.
//
// The second copy is UNKNOWN, with no level and the floor no bound on it,
// when the fit cannot tell it: when the recording holds so little of the
// reference's copy at some lag searched that a copy as loud as the floor
// would not stand out there, when the two paths' fit is singular, or when
// the first path's lags can reproduce most of what the reference gives the
// second's (minOwn).
func findSecond(ref, rec Audio, c Copy, k, lag, first int, h []float64, after int) (Copy, error) {
	x, y := ref.Samples, rec.Samples
	taps := len(h)
	lo := first + taps // the first lag after the first path
	if lag+after > k || k-taps+1 < lo {
		return Copy{}, fmt.Errorf("the first copy is %v late: within ±%v there is no room for a second path %v after it",
			ref.At(lag)-ref.Start, ref.At(k)-ref.Start, ref.At(after)-ref.Start)
	}
	left := filter(x, h, first, len(y))
	for i, v := range y {
		left[i] = v - left[i] // the recording less the first copy
	}
	r := correlate(x, left, lo, k)
	peak := lag + after
	for l := peak; l <= k; l++ {
		if math.Abs(r[l-lo]) > math.Abs(r[peak-lo]) {
			peak = l
		}
	}
	second := min(max(peak-taps/2, lo), k-taps+1) // the second path's first lag

	// The samples of the recording from the first path's first lag, where the
	// first copy can start, to where the second path's last lag reaches the
	// reference's end.
	m0, m1 := max(0, first), min(len(y), len(x)+second+taps-1)
	if m1-m0 < 8*taps {
		return Copy{}, fmt.Errorf("the files overlap by %d samples at both copies' lags: too few to measure", max(0, m1-m0))
	}
	n := 2 * taps
	g := gram(x, m0, m1, first, second, taps)
	// The reference's energy at the first copy's lag that the recording
	// holds: those samples are all in the fit's, and FindCopy found some of
	// it not silence.
	refEnergy := g[(lag-first)*n+lag-first]
	// noise is the share of the energy that neither path explains that the
	// second path takes when fitted to noise alone, at the strongest of the
	// lags searched.
	noise := (float64(taps) + 2*math.Log(float64(k-lag-after+1))) / float64(m1-m0)
	c.Second = &SecondCopy{}
	// A copy as loud as the floor puts the floor's share of the reference's
	// energy that the recording holds at its lag into the recording: it must
	// stand out at each lag searched, or a copy at that lag could be louder
	// than the floor and unseen.
	if leastHeld(x, len(y), lag+after, k) < foundFactor*noise*refEnergy {
		c.Second.Unknown = "the recording holds too little of the reference's copy at some lag searched for a copy as loud as the noise floor to stand out there"
		return c, nil
	}
	cross := append(sums(y[m0:m1], x, m0, first, taps), sums(y[m0:m1], x, m0, second, taps)...)
	normal := slices.Clone(g)
	for i := range n {
		normal[i*n+i] += 1e-9 * g[0]
	}
	paths, err := solveSPD(normal, cross)
	if err != nil {
		c.Second.Unknown = "the two copies' paths have no least-squares fit: " + err.Error()
		return c, nil
	}
	if own := ownShare(normal, g, taps); !(own >= minOwn) {
		c.Second.Unknown = fmt.Sprintf("the direct copy's path can reproduce %.0f %% of what the reference gives the second copy's lags, "+
			"as it can a steady tone's or a constant's, so the fit cannot tell the two copies apart", 100*(1-own))
		return c, nil
	}
	var fitted float64
	for i, v := range paths {
		fitted += v * cross[i]
	}
	// pathEnergy is the energy of path p's copy: its taps against its block of g.
	pathEnergy := func(p int) float64 {
		var e float64
		for i := range taps {
			for j := range taps {
				e += paths[p*taps+i] * g[(p*taps+i)*n+p*taps+j] * paths[p*taps+j]
			}
		}
		return e
	}
	unexplained := max(energy(y[m0:m1])-fitted, 0)
	c.LevelDB = 10 * math.Log10(pathEnergy(0)/refEnergy)
	c.FloorDB = 10 * math.Log10(unexplained/refEnergy)
	copied := pathEnergy(1)
	if copied <= foundFactor*noise*unexplained {
		return c, nil
	}
	at := taps + peak - second // the second copy's lag among the normal equations': its diagonal entry is the reference's energy the recording holds there
	*c.Second = SecondCopy{Found: true, Lag: ref.At(peak) - ref.Start, LevelDB: 10 * math.Log10(copied/g[at*n+at])}
	return c, nil
}

// leastHeld is, over the lags from lo to hi, the least energy of x whose
// copy at that lag a recording of ny samples holds: the energy of the
// samples i of x for which i+lag is a sample of the recording. One lag
// later, each end of those samples is one sample earlier, or stays.
func leastHeld(x []float32, ny, lo, hi int) float64 {
	clamp := func(i int) int { return min(max(i, 0), len(x)) }
	from, to := clamp(-lo), clamp(ny-lo)
	before, upTo := energy(x[:from]), energy(x[:to]) // the energy of x before each end
	least := math.Inf(1)
	for l := lo; ; l++ {
		least = min(least, max(upTo-before, 0))
		if l == hi {
			return least
		}
		if f := clamp(-l - 1); f < from {
			from = f
			before -= float64(x[from]) * float64(x[from])
		}
		if e := clamp(ny - l - 1); e < to {
			to = e
			upTo -= float64(x[to]) * float64(x[to])
		}
	}
}

// ownShare is, of the energy that the reference gives the second path's lags
// in the normal equations, the share that the first path's lags cannot
// reproduce: the trace of the second block once the first path is fitted
// (its Schur complement), against the block's own trace in g. l is the
// normal equations' Cholesky factor, which solveSPD leaves: the complement
// is the product of the factor's second diagonal block with its transpose,
// whose trace is the sum of that block's squares.
func ownShare(l, g []float64, taps int) float64 {
	n := 2 * taps
	var own, all float64
	for i := taps; i < n; i++ {
		for j := taps; j <= i; j++ {
			own += l[i*n+j] * l[i*n+j]
		}
		all += g[i*n+i]
	}
	return own / all
}

// gram is the matrix of the normal equations of two paths of taps each,
// over the lags from first and from second, over the samples m0 to m1 of
// the recording, x taken as 0 outside its own samples: for each two lags a
// and b, the sum of x[m-a]·x[m-b], the first path's lags first, row-major.
// Each block's first row and column are cross-correlations (sums); each
// entry after them is the one before it on its diagonal with the sum moved
// one sample, which keeps it exact.
func gram(x []float32, m0, m1, first, second, taps int) []float64 {
	n := 2 * taps
	g := make([]float64, n*n)
	starts := [2]int{first, second}
	for p := range 2 {
		for q := p; q < 2; q++ {
			a, b := starts[p], starts[q]
			ua, fa := lagged(x, m0, m1, a)
			ub, fb := lagged(x, m0, m1, b)
			row, col := sums(ua, x, fa, b, taps), sums(ub, x, fb, a, taps)
			for i := range taps {
				for j := range taps {
					var v float64
					switch {
					case i == 0:
						v = row[j]
					case j == 0:
						v = col[i]
					default:
						u, w := a+i-1, b+j-1 // the entry before, the sum over m of x[m-u]·x[m-w]
						v = g[(p*taps+i-1)*n+q*taps+j-1] +
							sample(x, m0-1-u)*sample(x, m0-1-w) - sample(x, m1-1-u)*sample(x, m1-1-w)
					}
					g[(p*taps+i)*n+q*taps+j] = v
					g[(q*taps+j)*n+p*taps+i] = v
				}
			}
		}
	}
	return g
}

// lagged is x[m-a] for the samples m from m0 to m1 where x has that sample,
// the rest being 0: those samples of x, and the m they start at.
func lagged(x []float32, m0, m1, a int) (u []float32, from int) {
	from, to := max(m0, a), min(m1, len(x)+a)
	if to <= from {
		return nil, m0
	}
	return x[from-a : to-a], from
}

// sample is x[i], or 0 where x has no sample i.
func sample(x []float32, i int) float64 {
	if i < 0 || i >= len(x) {
		return 0
	}
	return float64(x[i])
}

// sums is, for each j below n, the sum over i of u[i]·x[m0+i-b-j], x taken
// as 0 outside its own samples: with u the samples from m0 of a sequence,
// its cross-correlation with x at each lag from b, over exactly those
// samples.
func sums(u, x []float32, m0, b, n int) []float64 {
	if len(u) == 0 {
		return make([]float64, n)
	}
	s := correlate(u, x, m0-b-n+1, m0-b)
	slices.Reverse(s)
	return s
}

// energy is the sum of the squares of x.
func energy(x []float32) float64 {
	var sum float64
	for _, s := range x {
		sum += float64(s) * float64(s)
	}
	return sum
}

// solveSPD solves a·h = b, a symmetric positive definite n×n (row-major),
// by Cholesky. It overwrites a: its lower triangle, the diagonal included,
// is then the factor l, lower triangular with l·lᵀ = a.
func solveSPD(a, b []float64) ([]float64, error) {
	n := len(b)
	for j := range n {
		d := a[j*n+j]
		for k := range j {
			d -= a[j*n+k] * a[j*n+k]
		}
		if d <= 0 {
			return nil, errors.New("the reference's autocorrelation is singular: no echo path fits")
		}
		d = math.Sqrt(d)
		a[j*n+j] = d
		for i := j + 1; i < n; i++ {
			s := a[i*n+j]
			for k := range j {
				s -= a[i*n+k] * a[j*n+k]
			}
			a[i*n+j] = s / d
		}
	}
	h := make([]float64, n)
	copy(h, b)
	for i := range n {
		for k := range i {
			h[i] -= a[i*n+k] * h[k]
		}
		h[i] /= a[i*n+i]
	}
	for i := n - 1; i >= 0; i-- {
		for k := i + 1; k < n; k++ {
			h[i] -= a[k*n+i] * h[k]
		}
		h[i] /= a[i*n+i]
	}
	return h, nil
}
