package voice

import (
	"encoding/binary"
	"math"
	"slices"
	"testing"
	"time"
)

// speechLoudness is the loudness of Wispr's speech, in LUFS: the median
// BS.1770 short-term (3 s) loudness of voice A's speech, Gemini
// gemini-3.8-flash-tts with the voice Kore, saying the ten sentences of
// internal/voice/tts's live test end to end (36.8 s). ffmpeg 6.1.1's ebur128
// filter read -15.0 LUFS over the 339 windows wholly inside the speech, from
// -17.0 to -12.6, and an integrated loudness of -14.3 LUFS.
const speechLoudness = -15.0

// playedSounds are the sounds as Wispr plays them.
func playedSounds() map[string][]byte {
	return map[string][]byte{"wake": WakeChime(), "answer": AnswerChime(), "start": StartChime(), "error": ErrorChime()}
}

// TestChimesAreShortAndDistinct fails if the answer sound lasts less than
// 600 ms or more than 800 ms or is not one rising run of notes played twice,
// another sound lasts more than 400 ms, a sound clicks at its ends, is empty,
// has another number of notes than its design, or has no bounds here.
// TestChimesAreAsLoudAsSpeech checks that each can be heard.
func TestChimesAreShortAndDistinct(t *testing.T) {
	bounds := map[string]struct {
		notes    int
		shortest time.Duration
		longest  time.Duration
	}{
		"wake":   {2, 0, 400 * time.Millisecond},
		"answer": {6, 600 * time.Millisecond, 800 * time.Millisecond},
		"start":  {1, 0, 400 * time.Millisecond},
		"error":  {2, 0, 400 * time.Millisecond},
	}
	played := playedSounds()
	for name, notes := range chimeNotes {
		b, ok := bounds[name]
		if !ok {
			t.Errorf("the %s sound has no bounds in this test", name)
			continue
		}
		x := samples(played[name])
		if len(x) == 0 {
			t.Fatalf("%s chime is empty, or not among playedSounds", name)
		}
		d := time.Duration(len(x)) * time.Second / time.Duration(ChimeFormat.SampleRate)
		if d < b.shortest || d > b.longest {
			t.Errorf("%s chime lasts %v, want %v to %v", name, d, b.shortest, b.longest)
		}
		const click = 200.0 / 32768 // of full scale
		if math.Abs(x[0]) > click || math.Abs(x[len(x)-1]) > click {
			t.Errorf("%s chime starts at %.4f and ends at %.4f of full scale, want near 0 (no click)", name, x[0], x[len(x)-1])
		}
		if len(notes) != b.notes {
			t.Errorf("%s chime has %d notes, want %d", name, len(notes), b.notes)
		}
	}
	answer := chimeNotes["answer"]
	run := answer[:len(answer)/2]
	rising := true
	for i := 1; i < len(run); i++ {
		rising = rising && run[i].hz > run[i-1].hz
	}
	if !rising || !slices.Equal(run, answer[len(answer)/2:]) {
		t.Errorf("the answer sound plays %v, want one rising run of notes twice", answer)
	}
}

// TestChimesAreAsLoudAsSpeech fails if a sound Wispr plays is, while it
// plays, more than 2 LU softer or louder than Wispr's speech, or its true peak
// is over -1 dBTP. A sound's loudness is BS.1770's over its own length.
// Short-term loudness would misread it: its 3 s window averages a sound of
// 300 ms with 2.7 s of silence, about 10 LU under what the room hears.
func TestChimesAreAsLoudAsSpeech(t *testing.T) {
	rate := ChimeFormat.SampleRate
	played := playedSounds()
	for name := range chimeNotes {
		pcm, ok := played[name]
		if !ok {
			t.Errorf("the %s sound is not among playedSounds", name)
			continue
		}
		x := samples(pcm)
		l, peak := loudness(x, rate), truePeak(x, rate)
		t.Logf("the %s sound: %.1f LUFS over its %d ms, true peak %.1f dBTP", name, l, len(x)*1000/rate, peak)
		if !(math.Abs(l-speechLoudness) <= 2) { // a NaN fails too
			t.Errorf("the %s sound is %.1f LUFS while it plays, want within 2 LU of speech's %.1f", name, l, speechLoudness)
		}
		if !(peak <= -1) {
			t.Errorf("the %s sound peaks at %.1f dBTP, want -1 or lower", name, peak)
		}
	}
}

// TestLoudnessMeter fails if the meter of TestChimesAreAsLoudAsSpeech
// misreads BS.1770's reference tone or tones its K-weighting cuts or lifts,
// drops its gates, or misses a peak between samples.
func TestLoudnessMeter(t *testing.T) {
	const rate = 24000
	tone := func(hz, dBFS float64, d time.Duration) []float64 {
		x := make([]float64, int(d.Seconds()*rate))
		for i := range x {
			x[i] = math.Pow(10, dBFS/20) * math.Sin(2*math.Pi*hz*float64(i)/rate+math.Pi/4)
		}
		return x
	}
	// A 0 dBFS sine at 997 Hz reads -3.01 LUFS (BS.1770-4); ffmpeg 6.1.1's
	// ebur128 read the 40 Hz and 4 kHz tones, made by its aevalsrc as
	// 0.1*sin(2*PI*hz*t+PI/4) at 24 kHz for 1 s.
	cases := []struct {
		name string
		x    []float64
		want float64
	}{
		{"1 s of 997 Hz at -20 dBFS", tone(997, -20, time.Second), -23.01},
		{"220 ms of it: one block", tone(997, -20, 220*time.Millisecond), -23.01},
		{"1 s of 40 Hz at -20 dBFS: the high-pass", tone(40, -20, time.Second), -29.2},
		{"1 s of 4 kHz at -20 dBFS: the high shelf", tone(4000, -20, time.Second), -19.7},
		// 7 blocks of the tone, 3 of both, and 7 of -53 LUFS that only the
		// relative gate drops; with them, it would read -26.0.
		{"1 s of it, then 1 s at -50 dBFS", append(tone(997, -20, time.Second), tone(997, -50, time.Second)...), -23.71},
		{"1 s at -80 dBFS: under the absolute gate", tone(997, -80, time.Second), math.Inf(-1)},
	}
	for _, c := range cases {
		// got == c.want covers -Inf, whose difference from itself is NaN; any
		// other NaN fails.
		if got := loudness(c.x, rate); got != c.want && !(math.Abs(got-c.want) <= 0.1) {
			t.Errorf("%s reads %.2f LUFS, want %.2f", c.name, got, c.want)
		}
	}
	// Sines at a quarter of the rate with their crests half way and an eighth
	// of the way between samples: the first's samples are 3 dB under its peak,
	// and oversampling only to 96 kHz misses the second's peak by 0.17 dB.
	for _, crest := range []float64{0.5, 0.125} {
		x := make([]float64, rate/2)
		for i := range x {
			x[i] = math.Pow(10, -6.0/20) * math.Cos(math.Pi/2*(float64(i)-crest))
		}
		ramp := rate / 50
		for i := range ramp {
			x[i] *= float64(i) / float64(ramp)
			x[len(x)-1-i] *= float64(i) / float64(ramp)
		}
		if got := truePeak(x, rate); !(math.Abs(got+6) <= 0.1) {
			t.Errorf("a sine at -6 dBFS with its crests %.3f of the way between samples has a true peak of %.2f dBTP, want -6.0", crest, got)
		}
	}
}

// samples reads PCM16 LE as samples of full scale 1.
func samples(pcm []byte) []float64 {
	x := make([]float64, len(pcm)/2)
	for i := range x {
		x[i] = float64(int16(binary.LittleEndian.Uint16(pcm[2*i:]))) / 32768
	}
	return x
}

// loudness is x's BS.1770-4 integrated loudness in LUFS, over its own
// length: the gated mean power of its K-weighted 400 ms blocks, 100 ms apart,
// or of one block of all of x when x is shorter. Blocks under -70 LUFS are
// dropped, then blocks 10 LU under the mean of the rest; it is -Inf when no
// block is left, or x is empty.
func loudness(x []float64, rate int) float64 {
	z := kWeighted(x, float64(rate))
	block, hop := min(len(z), rate*4/10), rate/10
	if block == 0 {
		return math.Inf(-1)
	}
	var powers []float64
	for start := 0; start+block <= len(z); start += hop {
		var sum float64
		for _, v := range z[start : start+block] {
			sum += v * v
		}
		powers = append(powers, sum/float64(block))
	}
	lufs := func(p float64) float64 { return -0.691 + 10*math.Log10(p) }
	gated := func(gate float64) float64 {
		var sum float64
		var n int
		for _, p := range powers {
			if lufs(p) > gate {
				sum += p
				n++
			}
		}
		if n == 0 {
			return math.Inf(-1)
		}
		return lufs(sum / float64(n))
	}
	absolute := gated(-70)
	return gated(max(-70, absolute-10))
}

// kWeighted filters x, sampled at rate, through BS.1770's K-weighting: its
// high shelf, then its high-pass, with the coefficients libebur128 derives
// for rate.
func kWeighted(x []float64, rate float64) []float64 {
	k := math.Tan(math.Pi * 1681.974450955533 / rate)
	q := 0.7071752369554196
	vh := math.Pow(10, 3.999843853973347/20)
	vb := math.Pow(vh, 0.4996667741545416)
	a0 := 1 + k/q + k*k
	shelf := biquad{
		b: [3]float64{(vh + vb*k/q + k*k) / a0, 2 * (k*k - vh) / a0, (vh - vb*k/q + k*k) / a0},
		a: [2]float64{2 * (k*k - 1) / a0, (1 - k/q + k*k) / a0},
	}
	k = math.Tan(math.Pi * 38.13547087602444 / rate)
	q = 0.5003270373238773
	a0 = 1 + k/q + k*k
	highPass := biquad{b: [3]float64{1, -2, 1}, a: [2]float64{2 * (k*k - 1) / a0, (1 - k/q + k*k) / a0}}
	return highPass.filter(shelf.filter(x))
}

// biquad is a second-order filter normalized so that a0 is 1: b holds b0, b1
// and b2, and a holds a1 and a2. filter runs it in direct form I from rest.
type biquad struct {
	b [3]float64
	a [2]float64
}

func (f biquad) filter(x []float64) []float64 {
	y := make([]float64, len(x))
	var x1, x2, y1, y2 float64
	for i, v := range x {
		y[i] = f.b[0]*v + f.b[1]*x1 + f.b[2]*x2 - f.a[0]*y1 - f.a[1]*y2
		x2, x1, y2, y1 = x1, v, y1, y[i]
	}
	return y
}

// truePeak is x's true peak in dBTP: its highest magnitude with x, sampled at
// rate, oversampled by a Hann-windowed sinc to 192 kHz or more, as BS.1770-4's
// true-peak meter does.
func truePeak(x []float64, rate int) float64 {
	const half = 32
	over := (192000 + rate - 1) / rate
	var peak float64
	for i := range x {
		peak = max(peak, math.Abs(x[i]))
		for p := 1; p < over; p++ {
			var s float64
			for k := max(-half+1, -i); k <= half && i+k < len(x); k++ {
				d := float64(p)/float64(over) - float64(k)
				s += x[i+k] * math.Sin(math.Pi*d) / (math.Pi * d) * (0.5 + 0.5*math.Cos(math.Pi*d/half))
			}
			peak = max(peak, math.Abs(s))
		}
	}
	return 20 * math.Log10(peak)
}
