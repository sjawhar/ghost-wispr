// Package local is the paused listener's wake detector (contract 7, `local`):
// it hears "Wispr, start recording" or "Ghost Wispr, start recording" in the
// microphone's audio while the unit is in the listening state, and nothing
// else. Its frames reach only the keyword spotter and a ring of at most 2 s,
// which is zeroed on every state change. It stores, sends and logs no audio and
// no words: its only output is that it heard the phrase.
package local

import "strings"

const (
	// SampleRate is the rate the listener captures at, and the spotter's.
	SampleRate = 16000
	// RingSamples is the ring's size: 2 s, the most audio the listener holds.
	// The phrase lasts about 1.5 s.
	RingSamples = 2 * SampleRate
	// tailSamples is the silence stage 2 decodes after the ring, so the
	// recognizer emits the phrase's last word (gap-GapKWS.md §4 step 5).
	tailSamples = SampleRate / 2
	// startMark is what stage 2's text must hold: "START" and "STAR", as the
	// recognizer often spells it, but neither "STOP" nor "STIR" (gap-GapKWS.md
	// §3.2: "Wispr, stop recording" fell from 6 in 80 to 0 in 80).
	startMark = "STAR"
)

// Thresholds are stage 1's boost and threshold for both phrases.
type Thresholds struct{ Boost, Threshold float64 }

// DefaultThresholds are the ones the listener runs with (gap-GapKWS.md §4
// step 4).
var DefaultThresholds = Thresholds{Boost: 1.0, Threshold: 0.25}

// Options vary the detector for a measurement (cmd/measure-wake); the
// listener runs with DefaultThresholds and nothing else.
type Options struct {
	Thresholds Thresholds
	// Decoys are keyword lines after the two phrases; a hit on a line whose
	// label starts with DECOY counts as no phrase.
	Decoys string
	// NoStageTwo counts every stage-1 hit, with no recognizer.
	NoStageTwo bool
	// Threads is how many threads each stage runs on; 0 means 1. Only the
	// control of the Pi's CPU check sets more (accept-voice.sh --step
	// listener-cpu).
	Threads int
}

// StopDecoys are "Wispr, stop recording" and "Ghost Wispr, stop recording" as
// decoy lines, for the comparison gap-GapKWS.md §3.2 measured.
const StopDecoys = "▁W H IS PER ▁ST O P ▁RE C OR D ING :1.0 #0.25 @DECOY_STOP\n" +
	"▁G HO S T ▁W H IS PER ▁ST O P ▁RE C OR D ING :1.0 #0.25 @DECOY_GHOST_STOP\n"

// Spotter is stage 1: a streaming keyword spotter for the two start phrases.
type Spotter interface {
	// Accept feeds samples, 16 kHz mono in [-1, 1), and reports whether a
	// start phrase ended in them.
	Accept(samples []float32) bool
	// Reset forgets every sample fed so far.
	Reset()
	Close()
}

// Verifier is stage 2: the same model as a plain recognizer, which decodes a
// stretch of samples as text.
type Verifier interface {
	Text(samples []float32) string
	Close()
}

// Detector runs the two stages on the microphone's buffers. It keeps the last
// 2 s of samples in a fixed ring for stage 2, and nothing else: each buffer is
// copied into the ring, converted for the spotter in scratch, and dropped.
type Detector struct {
	spot   Spotter
	verify Verifier
	ring   [RingSamples]int16
	next   int // where the next sample goes
	filled int // samples in the ring, at most RingSamples
	// scratch converts samples for either stage; it is as long as stage 2's
	// input, the ring and its silence, and never grows.
	scratch []float32
	// hits counts stage-1 hits, confirmed or not, for a measurement.
	hits int
}

// StageOneHits is how many stage-1 hits the detector has had, confirmed or
// not: a measurement's count of what stage 2 refused.
func (d *Detector) StageOneHits() int { return d.hits }

// NewDetectorOf makes a detector of its two stages: the sherpa-onnx ones
// (NewDetector), or a test's.
func NewDetectorOf(spot Spotter, verify Verifier) *Detector {
	return &Detector{spot: spot, verify: verify, scratch: make([]float32, 0, RingSamples+tailSamples)}
}

// Write feeds one microphone buffer, 16 kHz mono PCM16, and reports whether
// it ended a start phrase that stage 2 confirmed. Any stage-1 hit, confirmed
// or not, zeroes the ring and resets the spotter, so the next hit reads new
// audio only.
func (d *Detector) Write(pcm []int16) bool {
	heard := false
	for len(pcm) > 0 {
		n := min(len(pcm), RingSamples)
		chunk := pcm[:n]
		pcm = pcm[n:]
		d.keep(chunk)
		samples := d.scratch[:n]
		for i, s := range chunk {
			samples[i] = float32(s) / 32768
		}
		if d.spot.Accept(samples) {
			d.hits++
			heard = heard || d.confirm()
			d.Reset()
		}
	}
	clear(d.scratch[:cap(d.scratch)])
	return heard
}

// keep copies chunk into the ring, over its oldest samples.
func (d *Detector) keep(chunk []int16) {
	for len(chunk) > 0 {
		n := copy(d.ring[d.next:], chunk)
		chunk = chunk[n:]
		d.next = (d.next + n) % RingSamples
		d.filled = min(d.filled+n, RingSamples)
	}
}

// confirm runs stage 2 on the ring, oldest sample first, then silence, and
// reports whether its text holds startMark.
func (d *Detector) confirm() bool {
	samples := d.scratch[:d.filled+tailSamples]
	start := (d.next - d.filled + RingSamples) % RingSamples
	for i := range d.filled {
		samples[i] = float32(d.ring[(start+i)%RingSamples]) / 32768
	}
	clear(samples[d.filled:])
	text := d.verify.Text(samples)
	return strings.Contains(text, startMark)
}

// Reset zeroes the ring and the conversion buffer, and resets the spotter's
// stream: the listener calls it at every state change, so no audio from one
// stretch of listening is heard in the next.
func (d *Detector) Reset() {
	clear(d.ring[:])
	clear(d.scratch[:cap(d.scratch)])
	d.next, d.filled = 0, 0
	d.spot.Reset()
}

// Close frees the spotter and the recognizer.
func (d *Detector) Close() {
	d.Reset()
	d.spot.Close()
	d.verify.Close()
}
