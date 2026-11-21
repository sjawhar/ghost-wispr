package audio

import "math"

// resampleTaps is how many input samples one output sample of an upsampling
// resampler is made from. The filter is a windowed sinc whose Blackman window
// keeps what lies past the cutoff more than 70 dB down; a downsampler widens
// it by the ratio, so that its cutoff is as sharp at the lower rate.
const resampleTaps = 32

// resampleCutoff is the filter's cutoff as a fraction of the lower rate's
// Nyquist frequency, below it so that the transition band ends near Nyquist:
// 11 kHz for 24 kHz audio.
const resampleCutoff = 0.92

// resampleMaxPhases bounds the filter table. Two rates with a large common
// factor need few phases (24 kHz to 48 kHz: 2; 44.1 kHz to 48 kHz: 160); two
// with none could need as many as the output rate, and then each output
// sample takes the nearest of resampleMaxPhases phases.
const resampleMaxPhases = 1024

// resampler converts mono audio from one rate to another and keeps its filter
// state between frames, so that frame boundaries add nothing. outputInput's
// render drives it: one output sample, then the input samples step asks for.
type resampler struct {
	from, to int
	taps     int
	phases   int
	coefs    []float32 // phases rows of taps
	// window holds the last taps input samples twice over, so that
	// window[pos:pos+taps] is the filter's input, oldest first. The sample
	// at the output's position, less its fraction, is the one taps/2-1
	// after the oldest.
	window []float32
	pos    int
	// frac is the output's position past that sample, in units of 1/to of
	// an input sample.
	frac int
}

// configure makes the resampler convert from one rate to another, starting
// from silence.
func (r *resampler) configure(from, to int) {
	r.from, r.to = from, to
	r.frac, r.pos = 0, 0
	if from == to {
		r.coefs, r.window = nil, nil
		return
	}
	r.taps = resampleTaps
	if from > to {
		r.taps = resampleTaps * ((from + to - 1) / to)
	}
	r.phases = min(to/gcd(from, to), resampleMaxPhases)
	cutoff := resampleCutoff * 0.5 * math.Min(1, float64(to)/float64(from)) // cycles per input sample
	half := float64(r.taps) / 2
	r.coefs = make([]float32, r.phases*r.taps)
	taps := make([]float64, r.taps)
	for p := range r.phases {
		delay := float64(p) / float64(r.phases)
		var sum float64
		for j := range taps {
			x := float64(j-(r.taps/2-1)) - delay // tap j's distance from the output, in input samples
			taps[j] = sinc(2*cutoff*x) * blackman(x/half)
			sum += taps[j]
		}
		for j, v := range taps {
			r.coefs[p*r.taps+j] = float32(v / sum) // unity gain at every phase
		}
	}
	r.window = make([]float32, 2*r.taps)
}

// reset returns the resampler to silence.
func (r *resampler) reset() {
	clear(r.window)
	r.pos, r.frac = 0, 0
}

// bypass reports whether the resampler passes samples through unchanged.
func (r *resampler) bypass() bool { return r.from == r.to }

// output is the output sample at the current position.
func (r *resampler) output() float32 {
	phase := r.frac * r.phases / r.to
	row := r.coefs[phase*r.taps : (phase+1)*r.taps]
	in := r.window[r.pos : r.pos+r.taps]
	var y float32
	for j, c := range row {
		y += c * in[j]
	}
	return y
}

// step moves to the next output sample and reports how many input samples
// it needs pushed first.
func (r *resampler) step() int {
	r.frac += r.from
	n := r.frac / r.to
	r.frac %= r.to
	return n
}

// push adds the next input sample.
func (r *resampler) push(s float32) {
	r.window[r.pos] = s
	r.window[r.pos+r.taps] = s
	if r.pos++; r.pos == r.taps {
		r.pos = 0
	}
}

// lag is how many input samples the resampler holds that have not reached
// the centre of its output: how far a pushed sample's output is delayed.
func (r *resampler) lag() int {
	if r.bypass() {
		return 0
	}
	return r.taps / 2
}

// reach is how many input samples a pushed sample stays in the filter: the
// end of a stream pushes this many of silence, so that the whole of the
// filter's response to its last sample plays.
func (r *resampler) reach() int {
	if r.bypass() {
		return 0
	}
	return r.taps
}

func sinc(x float64) float64 {
	if x == 0 {
		return 1
	}
	return math.Sin(math.Pi*x) / (math.Pi * x)
}

// blackman is the Blackman window over (-1, 1).
func blackman(u float64) float64 {
	if u <= -1 || u >= 1 {
		return 0
	}
	return 0.42 + 0.5*math.Cos(math.Pi*u) + 0.08*math.Cos(2*math.Pi*u)
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}
