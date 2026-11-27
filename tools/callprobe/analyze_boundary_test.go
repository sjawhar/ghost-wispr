package main

import (
	"math"
	"strconv"
	"strings"
	"testing"
)

// Bad options must not panic or print plausible measurements. In particular
// NaN thresholds make every comparison false and label noise as a sound.
func TestAnalysisRefusesInvalidMeasurementOptions(t *testing.T) {
	const rate = 16000
	dir := t.TempDir()
	ref := writeWAV(t, dir, "ref.wav", rate, noise(rate, -20, 101))
	in := writeWAV(t, dir, "in.wav", rate, noise(rate, -60, 102))
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"negative lag", []string{"delay", "-ref", ref, "-in", in, "-max-lag", "-1s"}},
		{"zero path", []string{"echo", "-ref", ref, "-in", in, "-path", "0"}},
		{"NaN sound threshold", []string{"ends", "-in", in, "-above", "NaN"}},
		{"infinite click threshold", []string{"clicks", "-in", in, "-above", "+Inf"}},
		{"negative window", []string{"ends", "-in", in, "-for", "-1s"}},
		{"negative quiet", []string{"clicks", "-in", in, "-quiet", "-1s"}},
		{"negative start", []string{"echo", "-ref", ref, "-in", in, "-start", "-1s"}},
		{"negative second-after", []string{"echo", "-ref", ref, "-in", in, "-second-after", "-20ms"}},
		{"a second-after shorter than the path", []string{"echo", "-ref", ref, "-in", in, "-second-after", "5ms"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if p := recover(); p != nil {
					t.Errorf("invalid measurement options panicked: %v", p)
				}
			}()
			out, errOut, code := callprobe(t, "", append([]string{"analyze"}, tc.args...)...)
			if code == 0 || out != "" || errOut == "" {
				t.Errorf("invalid measurement options gave code=%d stdout=%q stderr=%q", code, out, errOut)
			}
		})
	}
}

// CR14: c-notice reads 10 minutes of the loopback and wants sounds=0, which
// a recording that holds 2 minutes would give. Each analysis prints the
// duration it read of each file, and refuses a window a file does not hold.
func TestAShortRecordingFailsALongerWindow(t *testing.T) {
	const rate = 16000
	dir := t.TempDir()
	short := writeWAV(t, dir, "loopback.wav", rate, noise(2*rate, -70, 103))
	ref := writeWAV(t, dir, "source.wav", rate, noise(2*rate, -20, 104))
	for _, tc := range []struct {
		name string
		args []string
		want map[string]float64 // the durations read; nil: refused
	}{
		{"ends, the whole file", []string{"ends", "-in", short}, map[string]float64{"in_duration_ms": 2000}},
		{"ends, a window it holds", []string{"ends", "-in", short, "-start", "500ms", "-for", "1s"}, map[string]float64{"in_duration_ms": 1000}},
		{"clicks, to its end", []string{"clicks", "-in", short, "-start", "1.5s"}, map[string]float64{"in_duration_ms": 500}},
		{"clicks against a reference", []string{"clicks", "-ref", ref, "-in", short, "-for", "1s"},
			map[string]float64{"in_duration_ms": 1000, "ref_duration_ms": 1000}},
		{"delay", []string{"delay", "-ref", ref, "-in", short, "-max-lag", "500ms"}, map[string]float64{"in_duration_ms": 2000, "ref_duration_ms": 2000}},
		{"echo", []string{"echo", "-ref", ref, "-in", short, "-max-lag", "500ms"}, map[string]float64{"in_duration_ms": 2000, "ref_duration_ms": 2000}},
		{"ends, 10 s of 2 s", []string{"ends", "-in", short, "-for", "10s"}, nil},
		{"ends, from its end", []string{"ends", "-in", short, "-start", "2s"}, nil},
		{"clicks, past its end", []string{"clicks", "-in", short, "-start", "1s", "-for", "1.5s"}, nil},
		{"clicks against a reference, past their end", []string{"clicks", "-ref", ref, "-in", short, "-for", "3s"}, nil},
		{"delay, past their end", []string{"delay", "-ref", ref, "-in", short, "-max-lag", "500ms", "-for", "3s"}, nil},
		{"echo, after their end", []string{"echo", "-ref", ref, "-in", short, "-max-lag", "500ms", "-start", "3s"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, errOut, code := callprobe(t, "", append([]string{"analyze"}, tc.args...)...)
			if tc.want == nil {
				if code != 1 || out != "" || !strings.Contains(errOut, "window") {
					t.Errorf("a window the file does not hold gave code=%d stdout=%q stderr=%q, want 1, nothing and the window named", code, out, errOut)
				}
				return
			}
			if code != 0 {
				t.Fatalf("analyze exited %d: %s%s", code, out, errOut)
			}
			for key, want := range tc.want {
				if got := number(t, out, key); got != want {
					t.Errorf("%s=%v, want %v:\n%s", key, got, want, out)
				}
			}
		})
	}
}

// TA7: strconv.ParseFloat reads NaN and Inf, and a NaN passes every
// tolerance check written |got - want| > tolerance. number takes neither,
// and takes a finite number.
func TestNumberTakesNoNaNOrInfinity(t *testing.T) {
	for _, v := range []string{"NaN", "nan", "+Inf", "-Inf", "inf", "Infinity"} {
		if f, err := parseNumber("x="+v, "x"); err == nil {
			t.Errorf("parseNumber took x=%s as %v, want a refusal", v, f)
		}
	}
	if f, err := parseNumber("x=-46.0", "x"); err != nil || f != -46 {
		t.Errorf("parseNumber(x=-46.0) = %v, %v; want -46", f, err)
	}
}

// TA7: no analysis prints NaN or an infinity, which a reader's check could
// pass. The noise floor of digital silence, an energy of exactly zero,
// prints as silence.
func TestAnalysisPrintsNoNaNOrInfinity(t *testing.T) {
	const rate = 16000
	dir := t.TempDir()
	silent := writeWAV(t, dir, "silent.wav", rate, make([]float64, 2*rate))
	source := writeWAV(t, dir, "source.wav", rate, noise(2*rate, -20, 105))
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"ends of silence", []string{"ends", "-in", silent}},
		{"clicks of silence", []string{"clicks", "-in", silent}},
		{"clicks of silence against silence", []string{"clicks", "-ref", silent, "-in", silent}},
		{"the delay of a copy in silence", []string{"delay", "-ref", source, "-in", silent, "-max-lag", "500ms"}},
		{"the echo of a file in itself", []string{"echo", "-ref", source, "-in", source, "-max-lag", "500ms"}},
		{"the second copy of a file in itself", []string{"echo", "-ref", source, "-in", source, "-max-lag", "500ms", "-second-after", "20ms"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, errOut, code := callprobe(t, "", append([]string{"analyze"}, tc.args...)...)
			if code != 0 {
				t.Fatalf("analyze exited %d: %s%s", code, out, errOut)
			}
			for _, m := range keyValue.FindAllStringSubmatch(out, -1) {
				if f, err := strconv.ParseFloat(m[2], 64); err == nil && (math.IsNaN(f) || math.IsInf(f, 0)) {
					t.Errorf("%s=%s is not a finite number:\n%s", m[1], m[2], out)
				}
			}
			if tc.args[0] == "ends" {
				if got, _ := value(out, "noise_floor_dbfs"); got != "silence" {
					t.Errorf("noise_floor_dbfs=%q for digital silence, want silence:\n%s", got, out)
				}
			}
		})
	}
}
