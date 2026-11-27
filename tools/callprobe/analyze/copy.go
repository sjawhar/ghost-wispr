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
	MaxLag time.Duration // the copy is looked for from MaxLag before the reference to MaxLag after, at the lags held
	Path   time.Duration // the length of the echo path fitted, centred on the loudest copy's lag
	// SecondAfter, when not 0, also looks for a second copy at least this
	// long after the first, the loudest, which is then the direct one: as
	// the copy of Wispr's voice that comes back through the office's
	// microphone comes after the one Ghost Wispr sends. It is at least Path,
	// so that the two paths do not overlap.
	SecondAfter time.Duration
}

// Copy is what FindCopy found of a reference in a recording. Its levels are
// energies against the reference's where the two files overlap, in dB.
type Copy struct {
	Silent  bool          // the recording is digital silence there: nothing can be told
	Unknown string        // why no lag can be searched, or "": then nothing else is known, not even the floor
	Held    []LagRange    // the lags searched: where the recording holds heldShare of the reference's copy, over four path lengths (not Silent, not Unknown)
	Found   bool          // a copy stands out of the cross-correlation's noise at a lag held
	Lag     time.Duration // the recording holds the copy this much later (Found only)
	LevelDB float64       // the copy (Found only): with a Second told, the first path's of the two fitted at once
	FloorDB float64       // the noise floor, which bounds each path at the lags held (pathFloor; not Silent): with a Second told, each path at the lags searched for it, with what neither path explains
	Second  *SecondCopy   // with CopyOptions.SecondAfter, when the first copy was found above the floor
}

// LagRange is the lags from From to To, both included.
type LagRange struct{ From, To time.Duration }

// SecondCopy is the loudest copy at least CopyOptions.SecondAfter after the
// first, its path fitted together with the first's. When the fit cannot
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
func (c *Copy) BelowFloor() bool { return c.LevelDB <= c.FloorDB }

// SecondBelowFloor says the same of a found second copy.
func (c *Copy) SecondBelowFloor() bool { return c.Second.LevelDB <= c.FloorDB }

// foundFactor is how many times the energy that noise alone would fit a
// copy's path must stand above it to be a copy. Noise fits a path of n taps,
// chosen at the strongest of K lags, about n + 2·ln(K) times its energy per
// sample; a factor of 4 is far above where noise reaches.
const foundFactor = 4

// heldShare is the least share of the reference's energy whose copy at a lag
// the recording must hold for FindCopy to search that lag. At each lag held,
// a copy's level is measured on at least half of the reference, and the
// noise of the gain that picks the loudest copy is at most √2 times what it
// is at a lag the recording holds all of. A lag that holds less cannot show
// that a copy is absent there, nor that a copy found elsewhere is louder.
const heldShare = 0.5

// FindCopy looks for the loudest copy of ref in rec at each lag within
// o.MaxLag that rec holds heldShare of ref's copy at, over at least four
// path lengths, and fits the echo path from ref to rec over o.Path around
// it by least squares. The loudest is the
// greatest gain: the cross-correlation at a lag against the energy of ref
// whose copy rec holds there, so that a louder copy rec holds in part is not
// passed over for a quieter one it holds whole. The copy's level is the
// energy the path carries, so an echo spread over several paths reads as
// their sum. The noise floor bounds each path of o.Path at the lags held
// with what of rec the fitted path does not explain (pathFloor): each copy
// at a lag held that the path misses raises it to the copy's level,
// wherever in rec its sound is, as an echo the fit misses (one that
// changes, or is not linear) raises it. The floor bounds a copy only at the
// lags held, Held; with no lag held, or when the files overlap too little
// for a copy as loud as the floor to stand out at each lag held, the copy
// is Unknown.
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
	whole := energy(x)
	if whole == 0 {
		return Copy{}, errors.New("the reference is digital silence")
	}
	r := correlate(x, y, -k, k)
	// The lags held, each run of them as the first lag and the last; among
	// them the loudest copy's; and the energy held at each and the least of
	// it, which the floor and the floor's bound need. A lag held also
	// overlaps by four path lengths, the least a path is fitted over.
	var runs [][2]int
	best := loudest{gain: -1}
	heldAt := make([]float64, 2*k+1) // from lag -k
	least, searched := math.Inf(1), 0
	eachHeld(x, len(y), -k, k, func(l int, held float64, overlaps int) {
		if held < heldShare*whole || overlaps < 4*taps {
			return
		}
		if n := len(runs); n > 0 && runs[n-1][1] == l-1 {
			runs[n-1][1] = l
		} else {
			runs = append(runs, [2]int{l, l})
		}
		searched++
		heldAt[l+k] = held
		least = min(least, held)
		best.offer(l, r[l+k], held)
	})
	if searched == 0 {
		return Copy{Unknown: fmt.Sprintf("the recording holds less than %.0f %% of the reference's copy, or fewer than %d samples of it, at each lag within ±%v",
			100*heldShare, 4*taps, o.MaxLag)}, nil
	}
	lag := best.lag
	first := min(max(lag-taps/2, -k), k-taps+1) // the path's first lag

	// Where the two overlap at the copy's lag. refEnergy is the energy held
	// there, at least heldShare of the reference's.
	from, to := overlap(x, y, lag)
	recEnergy, refEnergy := energy(y[from+lag:to+lag]), energy(x[from:to])
	if recEnergy == 0 {
		return Copy{Silent: true}, nil
	}
	// noise is the share of the rest that a path fitted to noise alone
	// takes, at the strongest of the lags searched. A copy as loud as the
	// floor puts the floor's share of the energy held at its lag into rec:
	// it must stand out at each lag held, for the floor to bound a copy
	// there.
	noise := (float64(taps) + 2*math.Log(float64(searched))) / float64(to-from)
	if least < foundFactor*noise*refEnergy {
		return Copy{Unknown: fmt.Sprintf("the files overlap by %d samples at the loudest copy's lag, too few for a copy as loud as the noise floor to stand out at each lag held", to-from)}, nil
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
	left := filter(x, path, first, len(y))
	for i, v := range y {
		left[i] = v - left[i] // the recording less the copy
	}
	var floor float64
	for _, run := range runs {
		floor = max(floor, pathFloor(len(x), left, heldAt[run[0]+k:run[1]+k+1], run[0], taps))
	}
	c := Copy{FloorDB: 10 * math.Log10(floor), Held: make([]LagRange, len(runs))}
	for i, run := range runs {
		c.Held[i] = LagRange{ref.At(run[0]) - ref.Start, ref.At(run[1]) - ref.Start}
	}
	if fitted <= foundFactor*noise*rest {
		return c, nil
	}
	c.Found = true
	c.Lag = ref.At(lag) - ref.Start
	c.LevelDB = 10 * math.Log10(fitted/refEnergy)
	if o.SecondAfter == 0 || c.BelowFloor() {
		return c, nil
	}
	if err := findSecond(ref, rec, &c, k, lag, first, path, ref.samples(o.SecondAfter)); err != nil {
		return Copy{}, err
	}
	return c, nil
}

// overlap is where x overlaps y when y holds x lag samples later: x[from:to].
func overlap(x, y []float32, lag int) (from, to int) {
	return max(0, -lag), min(len(x), len(y)-lag)
}

// pathFloor is the noise floor that bounds each path of up to taps lags
// among the lags from lo, at which the recording holds the energy held[i]
// of the copy of a reference of nx samples at lag lo+i: the greatest, over
// each such path, of the energy of rest, the recording less what the fit
// explains, at the samples that a copy at the path's lags reaches, against
// the least energy held at those lags. A copy at those lags that the fit
// does not explain puts all the energy it holds into rest at those
// samples, wherever in the recording its sound is, so the floor is at least
// its level; and noise alone puts in its own level. A copy at the lags l
// reaches the samples from l to nx+l-1: one lag later, each end of a path's
// samples is one sample later, or stays.
func pathFloor(nx int, rest []float32, held []float64, lo, taps int) float64 {
	w := min(taps, len(held))
	clamp := func(i int) int { return min(max(i, 0), len(rest)) }
	var worst, before, upTo float64 // before and upTo, the energy of rest before each end of the path's samples
	from, to := 0, 0
	var least []int // the path's lags from the one held least, each held less than those after it, by index into held
	for i, h := range held {
		for len(least) > 0 && held[least[len(least)-1]] >= h {
			least = least[:len(least)-1]
		}
		least = append(least, i)
		f := i - w + 1 // the path's first lag, from lo; its last is i
		if f < 0 {
			continue
		}
		if least[0] < f {
			least = least[1:]
		}
		for end := clamp(lo + f); from < end; from++ {
			before += float64(rest[from]) * float64(rest[from])
		}
		for end := clamp(nx + lo + i); to < end; to++ {
			upTo += float64(rest[to]) * float64(rest[to])
		}
		worst = max(worst, (upTo-before)/held[least[0]])
	}
	return worst
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

// findSecond finds the second copy for FindCopy, into c: the loudest copy at
// least after samples after the first, found at lag with its path h over the
// lags from first. It takes the first copy out of the recording and finds,
// from lag+after, the lag of the greatest gain left: the cross-correlation
// against the reference's energy whose copy the recording holds there, so
// that a louder copy the recording holds in part is not passed over for a
// quieter one it holds whole. It then fits both paths at once by least
// squares, so that a reference that correlates with itself between the two
// lags, as a voice's pitch does, still has each copy's
// energy go to its own path. The fit is over every sample of the recording
// that either path reaches with a sample of the reference, the reference
// taken as silence outside its own: from the first copy's start to the
// second's end, past the reference's end, which is where the copy of a
// sound at the reference's end is. The levels are the energies of each
// path's copy over those samples, against the reference's energy that the
// recording holds at each copy's lag. The floor bounds each path at the
// lags searched, from lag+after, with what neither path explains
// (pathFloor): a copy there past the fit's samples raises it too.
//
// The second copy is UNKNOWN, with no level and the floor no bound on it,
// when the fit cannot tell it: when the recording holds so little of the
// reference's copy at some lag searched that a copy as loud as the floor
// would not stand out there, when the two paths' fit is singular, or when
// the first path's lags can reproduce most of what the reference gives the
// second's (minOwn).
func findSecond(ref, rec Audio, c *Copy, k, lag, first int, h []float64, after int) error {
	x, y := ref.Samples, rec.Samples
	taps := len(h)
	lo := first + taps // the first lag after the first path
	if lag+after > k || k-taps+1 < lo {
		return fmt.Errorf("the first copy is %v late: within ±%v there is no room for a second path %v after it",
			ref.At(lag)-ref.Start, ref.At(k)-ref.Start, ref.At(after)-ref.Start)
	}
	left := filter(x, h, first, len(y))
	for i, v := range y {
		left[i] = v - left[i] // the recording less the first copy
	}
	r := correlate(x, left, lo, k)
	// The loudest copy from lag+after; and the energy the recording holds of
	// the reference's copy at each of those lags and the least of it, which
	// the floor and the floor's bound need.
	best, least := loudest{lag: lag + after, gain: -1}, math.Inf(1)
	heldAt := make([]float64, 0, k-lag-after+1)
	eachHeld(x, len(y), lag+after, k, func(l int, held float64, _ int) {
		heldAt = append(heldAt, held)
		least = min(least, held)
		if held > 0 {
			best.offer(l, r[l-lo], held)
		}
	})
	peak := best.lag
	second := min(max(peak-taps/2, lo), k-taps+1) // the second path's first lag

	// The samples of the recording from the first path's first lag, where the
	// first copy can start, to where the second path's last lag reaches the
	// reference's end.
	m0, m1 := max(0, first), min(len(y), len(x)+second+taps-1)
	if m1-m0 < 8*taps {
		return fmt.Errorf("the files overlap by %d samples at both copies' lags: too few to measure", max(0, m1-m0))
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
	if least < foundFactor*noise*refEnergy {
		c.Second.Unknown = "the recording holds too little of the reference's copy at some lag searched for a copy as loud as the noise floor to stand out there"
		return nil
	}
	cross := append(sums(y[m0:m1], x, m0, first, taps), sums(y[m0:m1], x, m0, second, taps)...)
	normal := slices.Clone(g)
	for i := range n {
		normal[i*n+i] += 1e-9 * g[0]
	}
	paths, err := solveSPD(normal, cross)
	if err != nil {
		c.Second.Unknown = "the two copies' paths have no least-squares fit: " + err.Error()
		return nil
	}
	if own := ownShare(normal, g, taps); !(own >= minOwn) {
		c.Second.Unknown = fmt.Sprintf("the direct copy's path can reproduce %.0f %% of what the reference gives the second copy's lags, "+
			"as it can a steady tone's or a constant's, so the fit cannot tell the two copies apart", 100*(1-own))
		return nil
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
	rest, later := filter(x, paths[:taps], first, len(y)), filter(x, paths[taps:], second, len(y))
	for i, v := range y {
		rest[i] = v - rest[i] - later[i] // the recording less both copies
	}
	c.FloorDB = 10 * math.Log10(pathFloor(len(x), rest, heldAt, lag+after, taps))
	copied := pathEnergy(1)
	if copied <= foundFactor*noise*unexplained {
		return nil
	}
	at := taps + peak - second // the second copy's lag among the normal equations': its diagonal entry is the reference's energy the recording holds there
	*c.Second = SecondCopy{Found: true, Lag: ref.At(peak) - ref.Start, LevelDB: 10 * math.Log10(copied/g[at*n+at])}
	return nil
}

// sameGain is how near two gains are taken as equal: well above the FFT's
// rounding of the cross-correlation, and far below any difference in level.
const sameGain = 1e-9

// loudest keeps, of the lags offered, the lag of the greatest gain, the
// cross-correlation there against the reference's energy whose copy the
// recording holds; of gains equal within sameGain, it keeps the lag held
// most. A constant delayed is the same constant, and a steady tone delayed
// by a whole number of its periods the same tone: each such lag has the
// same gain, and the one held most measures it best.
type loudest struct {
	lag        int
	gain, held float64
}

// offer offers lag, where the cross-correlation is r and the energy held is
// held, above 0.
func (b *loudest) offer(lag int, r, held float64) {
	g := math.Abs(r) / held
	if g > b.gain*(1+sameGain) || (g >= b.gain*(1-sameGain) && held > b.held) {
		*b = loudest{lag, g, held}
	}
}

// eachHeld calls f for each lag from lo to hi, in order, with the energy of
// x whose copy at that lag a recording of ny samples holds, and how many
// samples of x that copy is: the samples i of x for which i+lag is a sample
// of the recording. One lag later, each end of those samples is one sample
// earlier, or stays.
func eachHeld(x []float32, ny, lo, hi int, f func(lag int, held float64, overlaps int)) {
	clamp := func(i int) int { return min(max(i, 0), len(x)) }
	from, to := clamp(-lo), clamp(ny-lo)
	before, upTo := energy(x[:from]), energy(x[:to]) // the energy of x before each end
	for l := lo; ; l++ {
		f(l, max(upTo-before, 0), max(to-from, 0))
		if l == hi {
			return
		}
		if b := clamp(-l - 1); b < from {
			from = b
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
