package analyze

import (
	"cmp"
	"errors"
	"math"
	"slices"
)

// minThreshold is the lowest threshold a sound or a click must reach, -80
// dBFS: about 3 steps of 16-bit PCM, so that a file of digital silence holds
// neither.
const minThreshold = 1e-4

// noiseFloor is a's noise floor, an RMS level at full scale 1: the 10th
// percentile of the RMS of its 10 ms frames. A recording quiet for less than
// a tenth of its length has its floor in its sound.
func noiseFloor(a Audio) (float64, error) {
	n := max(1, a.Rate/100)
	frames := len(a.Samples) / n
	if frames == 0 {
		return 0, errors.New("shorter than 10 ms: no noise floor")
	}
	rms := make([]float64, frames)
	for f := range rms {
		var sum float64
		for _, s := range a.Samples[f*n : (f+1)*n] {
			sum += float64(s) * float64(s)
		}
		rms[f] = math.Sqrt(sum / float64(n))
	}
	slices.Sort(rms)
	return Percentile(rms, 10), nil
}

// threshold is the level floor plus above dB, and at least minThreshold.
func threshold(floor, above float64) float64 {
	return max(floor*math.Pow(10, above/20), minThreshold)
}

// DBFS is an amplitude at full scale 1 in dB.
func DBFS(amplitude float64) float64 { return 20 * math.Log10(amplitude) }

// Percentile is the nearest-rank p-th percentile of sorted, which holds at
// least one value: the smallest value with at least p percent of the values
// at or below it.
func Percentile[T cmp.Ordered](sorted []T, p float64) T {
	i := int(math.Ceil(p*float64(len(sorted))/100)) - 1
	return sorted[max(0, min(i, len(sorted)-1))]
}
