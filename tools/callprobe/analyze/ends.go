package analyze

import (
	"errors"
	"math"
	"time"
)

// SoundOptions are what makes a stretch of a recording a sound.
type SoundOptions struct {
	Above float64       // dB by which a sound's 1 ms level stands above the noise floor
	Gap   time.Duration // a quieter stretch shorter than this joins the sounds on each side
	Min   time.Duration // a sound shorter than this is none
}

// Sound is one stretch of a recording above the threshold, its times from the
// file's start.
type Sound struct {
	Start, End time.Duration
	PeakDBFS   float64
}

// Sounds is what Ends found.
type Sounds struct {
	FloorDBFS, ThresholdDBFS float64
	Sounds                   []Sound
}

// Ends finds where each sound of a starts and ends: the stretches whose level
// over 1 ms, centred on each 0.1 ms, reaches the threshold. A sound's edge is
// found within half that millisecond, late at its end and early at its start.
func Ends(a Audio, o SoundOptions) (Sounds, error) {
	if math.IsNaN(o.Above) || math.IsInf(o.Above, 0) || o.Gap < 0 || o.Min < 0 {
		return Sounds{}, errors.New("above must be finite, and gap and min must be non-negative")
	}
	floor, err := noiseFloor(a)
	if err != nil {
		return Sounds{}, err
	}
	limit := threshold(floor, o.Above)
	found := Sounds{FloorDBFS: DBFS(floor), ThresholdDBFS: DBFS(limit)}
	window, hop := max(1, a.Rate/1000), max(1, a.Rate/10000)
	gap, shortest := a.samples(o.Gap), a.samples(o.Min)
	var runs [][2]int // each sound's first and last loud centre, as sample indexes
	for c := 0; c < len(a.Samples); c += hop {
		from := max(0, c-window/2)
		to := min(len(a.Samples), from+window)
		var sum float64
		for _, s := range a.Samples[from:to] {
			sum += float64(s) * float64(s)
		}
		if math.Sqrt(sum/float64(to-from)) < limit {
			continue
		}
		if n := len(runs); n > 0 && c-runs[n-1][1] <= max(gap, hop) {
			runs[n-1][1] = c
			continue
		}
		runs = append(runs, [2]int{c, c})
	}
	for _, r := range runs {
		if r[1]-r[0] < shortest {
			continue
		}
		var peak float32
		for _, s := range a.Samples[r[0] : r[1]+1] {
			peak = max(peak, s, -s)
		}
		found.Sounds = append(found.Sounds, Sound{Start: a.At(r[0]), End: a.At(r[1]), PeakDBFS: DBFS(float64(peak))})
	}
	return found, nil
}
