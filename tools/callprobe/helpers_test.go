package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The tests run the command as a person runs it, through run, on WAV files
// they make: a known signal, with a known delay, level or end, in known
// noise. Each expected value is the one the test put in the file.

// callprobe runs the command with args and stdin, and returns what it printed
// and its exit code.
func callprobe(t *testing.T, stdin string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(args, strings.NewReader(stdin), &out, &errOut)
	return out.String(), errOut.String(), code
}

// writeWAV writes samples, full scale ±1, to dir/name as a mono 16-bit PCM
// WAV file at rate, the format callprobe record has ffmpeg write, and
// returns its path.
func writeWAV(t *testing.T, dir, name string, rate int, samples []float64) string {
	t.Helper()
	data := make([]byte, 0, 2*len(samples))
	for _, s := range samples {
		v := max(-32768, min(32767, math.Round(s*32767)))
		data = binary.LittleEndian.AppendUint16(data, uint16(int16(v)))
	}
	b := []byte("RIFF")
	b = binary.LittleEndian.AppendUint32(b, uint32(36+len(data)))
	b = append(b, "WAVEfmt "...)
	b = binary.LittleEndian.AppendUint32(b, 16)
	b = binary.LittleEndian.AppendUint16(b, 1) // PCM
	b = binary.LittleEndian.AppendUint16(b, 1) // mono
	b = binary.LittleEndian.AppendUint32(b, uint32(rate))
	b = binary.LittleEndian.AppendUint32(b, uint32(2*rate))
	b = binary.LittleEndian.AppendUint16(b, 2)
	b = binary.LittleEndian.AppendUint16(b, 16)
	b = append(b, "data"...)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(data)))
	b = append(b, data...)
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// noise is n samples of white Gaussian noise whose RMS is level dBFS; the
// same seed gives the same noise.
func noise(n int, level float64, seed uint64) []float64 {
	r := rand.New(rand.NewPCG(seed, 1))
	a := amplitude(level)
	x := make([]float64, n)
	for i := range x {
		x[i] = a * r.NormFloat64()
	}
	return x
}

// amplitude is the amplitude ratio of a level in dB.
func amplitude(db float64) float64 { return math.Pow(10, db/20) }

// addCopy adds gain times src, delay samples later, into dst, as far as dst
// reaches.
func addCopy(dst, src []float64, delay int, gain float64) {
	for i, s := range src {
		if j := i + delay; j >= 0 && j < len(dst) {
			dst[j] += gain * s
		}
	}
}

// click adds a click of peak amp at sample at: an impulse that rings out in
// 16 samples, as a speaker's click does.
func click(x []float64, at int, amp float64) {
	a := amp
	for k := range 16 {
		x[at+k] += a
		a *= -0.7
	}
}

// keyValue is one key=value field of callprobe's output.
var keyValue = regexp.MustCompile(`([a-z_0-9]+)=([^\s()]+)`)

// value is the value of the first field named key in out.
func value(out, key string) (string, bool) {
	for _, m := range keyValue.FindAllStringSubmatch(out, -1) {
		if m[1] == key {
			return m[2], true
		}
	}
	return "", false
}

// number is the value of the first field named key in out, as a number; the
// test stops if out holds none.
func number(t *testing.T, out, key string) float64 {
	t.Helper()
	f, err := parseNumber(out, key)
	if err != nil {
		t.Fatalf("%v:\n%s", err, out)
	}
	return f
}

// parseNumber is the value of the first field named key in out, as a
// finite number: strconv.ParseFloat also reads NaN and Inf, and a NaN passes
// every tolerance check written |got - want| > tolerance.
func parseNumber(out, key string) (float64, error) {
	v, ok := value(out, key)
	if !ok {
		return 0, fmt.Errorf("the output holds no %s", key)
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("%s=%s is not a finite number", key, v)
	}
	return f, nil
}

// fakePath makes PATH a directory that holds only bash and the given fake
// programs, each a bash script, and points the PulseAudio client library at
// a socket that does not exist, so that no test reaches a real sound server
// or a real pactl or ffmpeg. It returns the directory.
func fakePath(t *testing.T, programs map[string]string) string {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatalf("the device tests run bash: %v", err)
	}
	dir := t.TempDir()
	if err := os.Symlink(bash, filepath.Join(dir, "bash")); err != nil {
		t.Fatal(err)
	}
	for name, body := range programs {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!"+bash+"\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	t.Setenv("PULSE_SERVER", "unix:"+filepath.Join(dir, "no-sound-server"))
	return dir
}

// lines is the non-empty lines of the file at path.
func lines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(string(data), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}
