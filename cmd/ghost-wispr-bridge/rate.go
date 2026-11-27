package main

import (
	"math"
	"time"
)

const (
	// rateWindow is the rate count's window (spec, "Failures and restarts").
	rateWindow = 10 * time.Second
	// rateGrace is how long after a window's end the clock closes it: audio
	// received just before the end may still wait in the audio queue.
	rateGrace = time.Second
	// rateLabel is the call mix's labelled rate; R10 holds it within 1 %.
	rateLabel = 24000
	rateSlack = rateLabel / 100
)

// rateCount counts the call's mixed audio samples against the bridge's clock,
// in back-to-back 10 s windows from the first sample. A PulseAudio at 44.1 kHz
// under a 48 kHz label reads about 22,050 (research-attendee-md section 1.6).
// Each window reports its own samples, a window with none reports 0, and a
// window closes on the clock as well as at the next sample.
type rateCount struct {
	start   time.Time // the open window's start; zero before the first sample
	samples int
	closed  []int // the closed windows' rates, reused
}

// add counts n samples received at at, and gives the rate a second of each
// window that at closed.
func (r *rateCount) add(at time.Time, n int) []int {
	if r.start.IsZero() {
		r.start = at
	}
	closed := r.close(at)
	if !at.Before(r.start) { // a sample of a window the clock closed is not counted
		r.samples += n
	}
	return closed
}

// close closes each window that ended at or before now, and gives the rate a
// second of each.
func (r *rateCount) close(now time.Time) []int {
	r.closed = r.closed[:0]
	if r.start.IsZero() {
		return nil
	}
	for end := r.start.Add(rateWindow); !now.Before(end); end = r.start.Add(rateWindow) {
		r.closed = append(r.closed, int(math.Round(float64(r.samples)/rateWindow.Seconds())))
		r.start, r.samples = end, 0
	}
	return r.closed
}

// rateFault: outside 24,000 a second, ± 1 %.
func rateFault(samplesPerSecond int) bool {
	return samplesPerSecond < rateLabel-rateSlack || samplesPerSecond > rateLabel+rateSlack
}
