package loudness

import (
	"math"
	"testing"
	"time"
)

// TestMeter fails if the meter misreads BS.1770's reference tone or tones its
// K-weighting cuts or lifts, drops its gates, or misses a peak between
// samples.
func TestMeter(t *testing.T) {
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
		if got := Integrated(c.x, rate); got != c.want && !(math.Abs(got-c.want) <= 0.1) {
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
		if got := TruePeak(x, rate); !(math.Abs(got+6) <= 0.1) {
			t.Errorf("a sine at -6 dBFS with its crests %.3f of the way between samples has a true peak of %.2f dBTP, want -6.0", crest, got)
		}
	}
}
