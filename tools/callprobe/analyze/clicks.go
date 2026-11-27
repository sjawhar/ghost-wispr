package analyze

import (
	"errors"
	"fmt"
	"math"
	"time"
)

// ClickOptions are what makes a sample a click.
type ClickOptions struct {
	Above float64       // dB by which a click's sample stands above the noise floor
	Quiet time.Duration // the time before a click in which no sample reaches the threshold
}

// Clicks is what FindClicks found.
type Clicks struct {
	FloorDBFS, ThresholdDBFS float64
	At                       []time.Duration // from the file's start
}

// FindClicks finds each click of a: the first sample at the threshold after
// o.Quiet of quiet. A sound that starts out of quiet starts with a click too,
// so the clicks to time are played over quiet.
func FindClicks(a Audio, o ClickOptions) (Clicks, error) {
	if math.IsNaN(o.Above) || math.IsInf(o.Above, 0) || o.Quiet < 0 {
		return Clicks{}, errors.New("above must be finite and quiet must be non-negative")
	}
	floor, err := noiseFloor(a)
	if err != nil {
		return Clicks{}, err
	}
	limit := threshold(floor, o.Above)
	found := Clicks{FloorDBFS: DBFS(floor), ThresholdDBFS: DBFS(limit)}
	quiet := a.samples(o.Quiet)
	last := -quiet - 1 // the last sample at the threshold
	for i, s := range a.Samples {
		if float64(max(s, -s)) < limit {
			continue
		}
		if i-last > quiet {
			found.At = append(found.At, a.At(i))
		}
		last = i
	}
	return found, nil
}

// ClickPair is a click of the reference and its copy in the recording, and
// how much later than the click, less the offset PairClicks was given, the
// recording holds its copy.
type ClickPair struct{ Ref, In, Delay time.Duration }

// PairClicks pairs each click of ref, moved by offset, with the nearest
// unpaired click of in within maxLag either side: offset is how much later
// in's time is than ref's for one moment, so that each delay is the path's
// alone. It refuses clicks of ref less than twice maxLag apart, whose
// windows would reach each other's click.
func PairClicks(ref, in []time.Duration, offset, maxLag time.Duration) ([]ClickPair, error) {
	for i := 1; i < len(ref); i++ {
		if ref[i]-ref[i-1] <= 2*maxLag {
			return nil, fmt.Errorf("the reference's clicks at %v and %v are %v apart: pairing within %v needs them more than %v apart",
				ref[i-1], ref[i], ref[i]-ref[i-1], maxLag, 2*maxLag)
		}
	}
	paired := make([]bool, len(in))
	var pairs []ClickPair
	for _, r := range ref {
		at, best := r+offset, -1
		for j, c := range in {
			if !paired[j] && abs(c-at) <= maxLag && (best < 0 || abs(c-at) < abs(in[best]-at)) {
				best = j
			}
		}
		if best >= 0 {
			paired[best] = true
			pairs = append(pairs, ClickPair{Ref: r, In: in[best], Delay: in[best] - at})
		}
	}
	return pairs, nil
}

func abs(d time.Duration) time.Duration { return max(d, -d) }
