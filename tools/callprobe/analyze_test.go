package main

import (
	"math"
	"strings"
	"testing"
)

// c-rig: on synthetic files, the analysis finds an injected delay of 137 ms
// within 2 ms, by cross-correlation and by click times (c-delay reads both:
// the median and p90 of 50 clicks).
func TestFindsAnInjectedDelayOf137msWithin2ms(t *testing.T) {
	const rate = 16000
	dir := t.TempDir()

	t.Run("cross-correlation", func(t *testing.T) {
		ref := noise(3*rate, -20, 1)
		in := noise(3*rate+rate/2, -50, 2)
		addCopy(in, ref, 137*rate/1000, 0.3)
		out, errOut, code := callprobe(t, "", "analyze", "delay",
			"-ref", writeWAV(t, dir, "ref.wav", rate, ref), "-in", writeWAV(t, dir, "in.wav", rate, in))
		if code != 0 {
			t.Fatalf("analyze delay exited %d: %s%s", code, out, errOut)
		}
		if got := number(t, out, "delay_ms"); math.Abs(got-137) > 2 {
			t.Errorf("delay_ms=%v, want 137 within 2 ms:\n%s", got, out)
		}
	})

	t.Run("click times", func(t *testing.T) {
		// Five clicks 2.5 s apart, each copy 137 ms later give or take its
		// own jitter: sorted 136, 137, 137, 138, 140, so the median is 137
		// and the p90 140.
		delays := []int{136, 137, 138, 137, 140}
		ref := noise(14*rate, -70, 3)
		in := noise(14*rate, -70, 4)
		for i, d := range delays {
			at := rate/4 + i*5*rate/2
			click(ref, at, 0.8)
			click(in, at+d*rate/1000, 0.3)
		}
		out, errOut, code := callprobe(t, "", "analyze", "clicks",
			"-ref", writeWAV(t, dir, "ref-clicks.wav", rate, ref), "-in", writeWAV(t, dir, "in-clicks.wav", rate, in))
		if code != 0 {
			t.Fatalf("analyze clicks exited %d: %s%s", code, out, errOut)
		}
		if got := number(t, out, "pairs"); got != 5 {
			t.Errorf("pairs=%v, want the 5 clicks paired:\n%s", got, out)
		}
		if got := number(t, out, "median_ms"); math.Abs(got-137) > 2 {
			t.Errorf("median_ms=%v, want 137 within 2 ms:\n%s", got, out)
		}
		if got := number(t, out, "p90_ms"); math.Abs(got-140) > 2 {
			t.Errorf("p90_ms=%v, want 140 within 2 ms:\n%s", got, out)
		}
	})
}

// A calibration plays a click heard in two recordings before and after a
// measurement: the offset between the recordings held only if each click's
// delay is the same. The spread, the largest delay less the smallest, says
// so: 52 ms less 50 ms here.
func TestClickDelaysGiveTheirSpread(t *testing.T) {
	const rate = 48000
	dir := t.TempDir()
	ref := noise(10*rate, -70, 15)
	in := noise(10*rate, -70, 16)
	for i, d := range []float64{50, 52, 50.5} {
		at := rate + i*3*rate
		click(ref, at, 0.8)
		click(in, at+int(d*rate/1000), 0.8)
	}
	out, errOut, code := callprobe(t, "", "analyze", "clicks",
		"-ref", writeWAV(t, dir, "loopback.wav", rate, ref), "-in", writeWAV(t, dir, "room.wav", rate, in))
	if code != 0 {
		t.Fatalf("analyze clicks exited %d: %s%s", code, out, errOut)
	}
	if got := number(t, out, "spread_ms"); math.Abs(got-2) > 0.1 {
		t.Errorf("spread_ms=%v, want 2:\n%s", got, out)
	}
}

// c-delay: two recordings start seconds apart, so a click's copy is
// calibration 1's offset plus the path's delay later in the other file. The
// clicks repeat every 3 s (the README's clicks.wav), and an offset of 4 s
// puts each copy 1.1 s after the next click: a pairing that ignores the
// offset finds nothing within 1 s, and within 1.4 s pairs each copy with the
// wrong click, 3 s off. -offset moves each click of -ref by the offset
// before pairing, so each copy pairs with its own click and each delay is
// the path's alone. A copy that pairs before its click is refused: the
// offset is wrong, or the copy is later than -max-lag and paired with the
// next click.
func TestClicksPairAfterTheCalibrationOffset(t *testing.T) {
	const rate = 16000
	const offset = 4000 // ms
	dir := t.TempDir()
	ref := noise(20*rate, -70, 30)
	in := noise(20*rate, -70, 31)
	// Sorted, the delays are 136, 137, 137, 138 and 140 ms: the median is
	// 137 and the p90 140.
	for i, d := range []int{136, 137, 138, 137, 140} {
		at := rate/2 + i*3*rate
		click(ref, at, 0.8)
		click(in, at+(offset+d)*rate/1000, 0.3)
	}
	refPath, inPath := writeWAV(t, dir, "virtual-mic.wav", rate, ref), writeWAV(t, dir, "room.wav", rate, in)

	for _, maxLag := range []string{"1s", "1.4s"} {
		t.Run("with the offset, pairing within "+maxLag, func(t *testing.T) {
			out, errOut, code := callprobe(t, "", "analyze", "clicks", "-ref", refPath, "-in", inPath,
				"-offset", "4s", "-max-lag", maxLag)
			if code != 0 {
				t.Fatalf("analyze clicks exited %d: %s%s", code, out, errOut)
			}
			if got := number(t, out, "offset_ms"); got != offset {
				t.Errorf("offset_ms=%v, want %v:\n%s", got, offset, out)
			}
			if got := number(t, out, "pairs"); got != 5 {
				t.Errorf("pairs=%v, want the 5 clicks paired:\n%s", got, out)
			}
			if got := number(t, out, "median_ms"); math.Abs(got-137) > 2 {
				t.Errorf("median_ms=%v, want 137 within 2 ms:\n%s", got, out)
			}
			if got := number(t, out, "p90_ms"); math.Abs(got-140) > 2 {
				t.Errorf("p90_ms=%v, want 140 within 2 ms:\n%s", got, out)
			}
		})
	}

	t.Run("an offset that puts each copy before its click", func(t *testing.T) {
		out, errOut, code := callprobe(t, "", "analyze", "clicks", "-ref", refPath, "-in", inPath, "-offset", "4.3s")
		if code != 1 {
			t.Errorf("analyze clicks exited %d with each copy 163 ms before its click, want 1:\n%s%s", code, out, errOut)
		}
		if !strings.Contains(errOut, "before") {
			t.Errorf("stderr does not say the copy comes before its click:\n%s", errOut)
		}
		for _, key := range []string{"delay_ms", "median_ms", "p90_ms"} {
			if v, ok := value(out, key); ok {
				t.Errorf("%s=%s for a refused pairing, want none:\n%s", key, v, out)
			}
		}
	})
}

// c-rig: the analysis finds the end of a tone within 2 ms (c-stop's tail is
// the end of Wispr's sound in the loopback), and its start (c-carry's head
// that Meet's unmute cut), at a high pitch and at a low one cut mid-cycle.
func TestFindsTheEndOfAToneWithin2ms(t *testing.T) {
	const rate = 48000
	dir := t.TempDir()
	for _, tc := range []struct {
		name            string
		hz, start, stop float64 // the tone's pitch, and its start and end in seconds
	}{
		{"1 kHz", 1000, 0.5, 1.7345},
		{"150 Hz cut mid-cycle", 150, 0.25, 1.2021},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := noise(5*rate/2, -70, 5)
			from, to := int(tc.start*rate), int(tc.stop*rate)
			for i := from; i < to; i++ {
				x[i] += 0.1 * math.Sin(2*math.Pi*tc.hz*float64(i-from)/rate)
			}
			out, errOut, code := callprobe(t, "", "analyze", "ends", "-in", writeWAV(t, dir, tc.name+".wav", rate, x))
			if code != 0 {
				t.Fatalf("analyze ends exited %d: %s%s", code, out, errOut)
			}
			if got := number(t, out, "sounds"); got != 1 {
				t.Fatalf("sounds=%v, want the one tone:\n%s", got, out)
			}
			if got, want := number(t, out, "end_ms"), tc.stop*1000; math.Abs(got-want) > 2 {
				t.Errorf("end_ms=%v, want %v within 2 ms:\n%s", got, want, out)
			}
			if got, want := number(t, out, "start_ms"), tc.start*1000; math.Abs(got-want) > 2 {
				t.Errorf("start_ms=%v, want %v within 2 ms:\n%s", got, want, out)
			}
		})
	}
}

// echoTap is one path of an injected echo: delay samples after the copy's
// first, at gain.
type echoTap struct {
	delay int
	gain  float64
}

// c-rig: an echo injected at -46 dB and at -52 dB against the source, in a
// noise floor 70 dB below it, reads within 1 dB (c-once's bound is 46 dB).
// An echo spread over three paths reads as their sum, not as its strongest
// path alone (-47.8 dB here), which would read an echo as quieter than it is.
func TestFindsAnEchoAtMinus46AndMinus52dBWithin1dB(t *testing.T) {
	const rate = 16000
	dir := t.TempDir()
	// Three paths of relative gains 1, 0.6 and -0.4, whose sum is -46 dB:
	// the first alone is 10*log10(1.52) = 1.82 dB below the sum.
	first := amplitude(-46) / math.Sqrt(1+0.36+0.16)
	for _, tc := range []struct {
		name string
		taps []echoTap
		want float64
	}{
		{"-46 dB", []echoTap{{0, amplitude(-46)}}, -46},
		{"-52 dB", []echoTap{{0, amplitude(-52)}}, -52},
		{"-46 dB over three paths", []echoTap{{0, first}, {8, 0.6 * first}, {20, -0.4 * first}}, -46},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := noise(5*rate, -20, 6)
			loopback := noise(5*rate+rate/2, -90, 7) // 70 dB below the source
			for _, tap := range tc.taps {
				addCopy(loopback, source, 180*rate/1000+tap.delay, tap.gain)
			}
			out, errOut, code := callprobe(t, "", "analyze", "echo",
				"-ref", writeWAV(t, dir, "source.wav", rate, source), "-in", writeWAV(t, dir, "loopback.wav", rate, loopback))
			if code != 0 {
				t.Fatalf("analyze echo exited %d: %s%s", code, out, errOut)
			}
			if got := number(t, out, "echo_db"); math.Abs(got-tc.want) > 1 {
				t.Errorf("echo_db=%v, want %v within 1 dB:\n%s", got, tc.want, out)
			}
			if got := number(t, out, "noise_floor_db"); math.Abs(got+70) > 1 {
				t.Errorf("noise_floor_db=%v, want -70 within 1 dB:\n%s", got, out)
			}
			if got := number(t, out, "delay_ms"); math.Abs(got-180) > 2 {
				t.Errorf("delay_ms=%v, want the echo's 180 ms within 2 ms:\n%s", got, out)
			}
		})
	}
}

// c-once: the loopback holds Wispr's voice twice, the copy Ghost Wispr sends
// and a later one through the office's microphone, and the later one must be
// at least 46 dB below the first (R11). echo -second-after takes the
// strongest copy as the direct one, finds the strongest copy at least that
// long after it, fits the two paths together, and gives each level and the
// second's against the first. A search of the cross-correlation alone fails
// here: over 5 s of white noise the direct copy's own correlation noise is
// 1/283 of its peak, more than a copy 46 dB (1/200) below it. A voice's
// pitch correlates the reference with itself 120 ms later: with a steady
// 200 Hz tone of 30 % of the reference's power, 24 periods in 120 ms,
// fitting each path in turn, twice, read the copy 1.5 dB low and the floor
// 2.5 dB high, so the two paths are fitted at once. A recording with the
// direct copy alone reads copy=none, and a copy below the floor reads
// UNKNOWN, never a level.
func TestFindsASecondCopyAtMinus46AndMinus52dBBelowTheDirectCopy(t *testing.T) {
	const rate = 16000
	const direct, directMS, copyMS = -6.0, 180, 300 // the direct copy's level against the source, and the delays
	dir := t.TempDir()
	for _, tc := range []struct {
		name    string
		copyDB  float64 // the second copy against the direct one; 0: none
		floorDB float64 // the noise against the direct copy
		pitch   bool    // the source has a steady tone
	}{
		{"-46 dB", -46, -70, false},
		{"-52 dB", -52, -70, false},
		{"-52 dB with a steady pitch", -52, -70, true},
		{"the direct copy alone", 0, -70, false},
		{"a copy below the floor", -52, -40, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := noise(5*rate, -16, 20)
			if tc.pitch {
				white, tone := math.Sqrt(0.7), amplitude(-16)*math.Sqrt(0.6) // 70 % and 30 % of the power
				for i := range source {
					source[i] = white*source[i] + tone*math.Sin(2*math.Pi*200*float64(i)/rate)
				}
			}
			loopback := noise(5*rate+rate/2, -16+direct+tc.floorDB, 21)
			addCopy(loopback, source, directMS*rate/1000, amplitude(direct))
			if tc.copyDB != 0 {
				addCopy(loopback, source, copyMS*rate/1000, amplitude(direct+tc.copyDB))
			}
			out, errOut, code := callprobe(t, "", "analyze", "echo", "-second-after", "20ms",
				"-ref", writeWAV(t, dir, "wispr.wav", rate, source), "-in", writeWAV(t, dir, "loopback.wav", rate, loopback))
			if code != 0 {
				t.Fatalf("analyze echo exited %d: %s%s", code, out, errOut)
			}
			near := func(key string, want, tolerance float64) {
				t.Helper()
				if got := number(t, out, key); math.Abs(got-want) > tolerance {
					t.Errorf("%s=%v, want %v within %v:\n%s", key, got, want, tolerance, out)
				}
			}
			near("direct_delay_ms", directMS, 2)
			near("direct_db", direct, 1)
			near("floor_vs_direct_db", tc.floorDB, 1)
			near("noise_floor_db", direct+tc.floorDB, 1)
			switch {
			case tc.copyDB == 0:
				if got, _ := value(out, "copy"); got != "none" {
					t.Errorf("copy=%q with the direct copy alone, want none:\n%s", got, out)
				}
				for _, key := range []string{"copy_delay_ms", "copy_db", "copy_vs_direct_db"} {
					if v, ok := value(out, key); ok {
						t.Errorf("%s=%s with the direct copy alone, want no %s:\n%s", key, v, key, out)
					}
				}
			case tc.copyDB > tc.floorDB:
				near("copy_delay_ms", copyMS, 2)
				near("copy_db", direct+tc.copyDB, 1)
				near("copy_vs_direct_db", tc.copyDB, 1)
			default:
				near("copy_delay_ms", copyMS, 2)
				for _, key := range []string{"copy_db", "copy_vs_direct_db"} {
					if got, _ := value(out, key); got != "UNKNOWN" {
						t.Errorf("%s=%q for a copy below the floor, want UNKNOWN:\n%s", key, got, out)
					}
				}
			}
		})
	}
}

// c-rig: an echo below the recording's noise floor reads UNKNOWN, never a
// level: at -52 dB in a floor at -40 dB, the copy is there, and no number
// says how loud.
func TestReportsUnknownForAnEchoBelowTheNoiseFloor(t *testing.T) {
	const rate = 16000
	dir := t.TempDir()
	source := noise(5*rate, -20, 8)
	loopback := noise(5*rate+rate/2, -60, 9) // 40 dB below the source
	addCopy(loopback, source, 180*rate/1000, amplitude(-52))
	out, errOut, code := callprobe(t, "", "analyze", "echo",
		"-ref", writeWAV(t, dir, "source.wav", rate, source), "-in", writeWAV(t, dir, "loopback.wav", rate, loopback))
	if code != 0 {
		t.Fatalf("analyze echo exited %d: %s%s", code, out, errOut)
	}
	if got, _ := value(out, "echo_db"); got != "UNKNOWN" {
		t.Errorf("echo_db=%q, want UNKNOWN for an echo below the noise floor:\n%s", got, out)
	}
	if got := number(t, out, "noise_floor_db"); math.Abs(got+40) > 1 {
		t.Errorf("noise_floor_db=%v, want -40 within 1 dB:\n%s", got, out)
	}
}

// A recording that holds nothing but digital silence cannot show that an
// echo is absent: a loopback nobody routed reads UNKNOWN, not as an echo
// loss without end.
func TestReportsUnknownForASilentRecording(t *testing.T) {
	const rate = 16000
	dir := t.TempDir()
	out, errOut, code := callprobe(t, "", "analyze", "echo",
		"-ref", writeWAV(t, dir, "source.wav", rate, noise(5*rate, -20, 10)),
		"-in", writeWAV(t, dir, "loopback.wav", rate, make([]float64, 5*rate)))
	if code != 0 {
		t.Fatalf("analyze echo exited %d: %s%s", code, out, errOut)
	}
	if got, _ := value(out, "echo_db"); got != "UNKNOWN" {
		t.Errorf("echo_db=%q, want UNKNOWN for a silent recording:\n%s", got, out)
	}
	if v, ok := value(out, "noise_floor_db"); ok {
		t.Errorf("noise_floor_db=%s for a silent recording, want none:\n%s", v, out)
	}
}

// c-rig's negative control: each analysis, given a file in which nothing was
// injected, reports none, so a tool that always finds something fails here.
func TestReportsNoDelayAndNoEchoWhenNoneWasInjected(t *testing.T) {
	const rate = 16000
	dir := t.TempDir()
	quiet := writeWAV(t, dir, "quiet.wav", rate, noise(5*rate+rate/2, -90, 11))
	source := writeWAV(t, dir, "source.wav", rate, noise(5*rate, -20, 12))
	clicks := noise(14*rate, -70, 13)
	for i := range 5 {
		click(clicks, rate/4+i*5*rate/2, 0.8)
	}
	clicked := writeWAV(t, dir, "clicks.wav", rate, clicks)

	for _, tc := range []struct {
		name  string
		args  []string
		none  string   // the field that says none
		want  string   // its value
		never []string // fields that only a finding has
	}{
		{"cross-correlation", []string{"delay", "-ref", source, "-in", quiet}, "delay", "none", []string{"delay_ms"}},
		{"click times against a reference", []string{"clicks", "-ref", clicked, "-in", quiet}, "pairs", "0", []string{"median_ms", "p90_ms", "delay_ms"}},
		{"click times", []string{"clicks", "-in", quiet}, "clicks", "0", []string{"at_ms"}},
		{"sound ends", []string{"ends", "-in", quiet}, "sounds", "0", []string{"start_ms", "end_ms"}},
		{"echo level", []string{"echo", "-ref", source, "-in", quiet}, "echo", "none", []string{"echo_db", "delay_ms"}},
		{"echo level in a loud floor", []string{"echo", "-ref", source, "-in",
			writeWAV(t, dir, "loud.wav", rate, noise(5*rate+rate/2, -60, 14))}, "echo", "none", []string{"echo_db", "delay_ms"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, errOut, code := callprobe(t, "", append([]string{"analyze"}, tc.args...)...)
			if code != 0 {
				t.Fatalf("analyze %s exited %d: %s%s", strings.Join(tc.args, " "), code, out, errOut)
			}
			if got, _ := value(out, tc.none); got != tc.want {
				t.Errorf("%s=%q, want %s where nothing was injected:\n%s", tc.none, got, tc.want, out)
			}
			for _, key := range tc.never {
				if v, ok := value(out, key); ok {
					t.Errorf("%s=%s where nothing was injected, want no %s:\n%s", key, v, key, out)
				}
			}
		})
	}
}
