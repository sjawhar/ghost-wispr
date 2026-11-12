package audio

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/hajimehoshi/go-mp3"
)

func requireFFmpeg(t *testing.T) {
	t.Helper()
	for _, binary := range []string{"ffmpeg", "ffprobe"} {
		if output, err := exec.Command(binary, "-version").CombinedOutput(); err != nil {
			t.Skipf("%s is not runnable: %v: %s", binary, err, strings.TrimSpace(string(output)))
		}
	}
}

func makeThreeToneMP3(t *testing.T, path string) {
	t.Helper()
	cmd := exec.Command(
		"ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100:duration=3",
		"-f", "lavfi", "-i", "sine=frequency=660:sample_rate=44100:duration=3",
		"-f", "lavfi", "-i", "sine=frequency=880:sample_rate=44100:duration=4",
		"-filter_complex", "[0:a][1:a][2:a]concat=n=3:v=0:a=1[out]",
		"-map", "[out]", "-q:a", "2", path,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("make three-tone MP3: %v\n%s", err, output)
	}
}

func ffprobeDuration(t *testing.T, path string) float64 {
	t.Helper()
	output, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "default=noprint_wrappers=1:nokey=1", path).Output()
	if err != nil {
		t.Fatalf("ffprobe duration: %v", err)
	}
	duration, err := strconv.ParseFloat(strings.TrimSpace(string(output)), 64)
	if err != nil {
		t.Fatalf("parse ffprobe duration %q: %v", output, err)
	}
	return duration
}

func decodeMP3LeftChannel(t *testing.T, path string) ([]float64, int) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open MP3: %v", err)
	}
	defer func() { _ = file.Close() }()

	decoder, err := mp3.NewDecoder(file)
	if err != nil {
		t.Fatalf("new MP3 decoder: %v", err)
	}
	raw, err := io.ReadAll(decoder)
	if err != nil {
		t.Fatalf("decode MP3: %v", err)
	}
	if len(raw)%4 != 0 {
		t.Fatalf("decoded MP3 has %d bytes, not stereo PCM frames", len(raw))
	}
	samples := make([]float64, 0, len(raw)/4)
	for offset := 0; offset < len(raw); offset += 4 {
		samples = append(samples, float64(int16(binary.LittleEndian.Uint16(raw[offset:]))))
	}
	return samples, decoder.SampleRate()
}

func goertzelPower(samples []float64, sampleRate int, frequency float64) float64 {
	omega := 2 * math.Pi * frequency / float64(sampleRate)
	coefficient := 2 * math.Cos(omega)
	var q0, q1, q2 float64
	for _, sample := range samples {
		q0 = coefficient*q1 - q2 + sample
		q2 = q1
		q1 = q0
	}
	return q1*q1 + q2*q2 - coefficient*q1*q2
}

func TestSpliceCutsTheSpan(t *testing.T) {
	requireFFmpeg(t)
	path := filepath.Join(t.TempDir(), "tones.mp3")
	makeThreeToneMP3(t, path)

	sha256, err := Splice(context.Background(), path, 3, 6)
	if err != nil {
		t.Fatalf("Splice: %v", err)
	}
	if sha256 == "" {
		t.Fatal("Splice returned an empty SHA-256")
	}
	if got := ffprobeDuration(t, path); math.Abs(got-7) > 0.05 {
		t.Fatalf("spliced duration = %.3fs, want 7.0 ± 0.05s", got)
	}

	samples, sampleRate := decodeMP3LeftChannel(t, path)
	power440 := goertzelPower(samples, sampleRate, 440)
	power660 := goertzelPower(samples, sampleRate, 660)
	power880 := goertzelPower(samples, sampleRate, 880)
	if power660 >= math.Min(power440, power880)/100 {
		t.Fatalf("middle 660Hz tone survives splice: 660Hz power %.0f, 440Hz %.0f, 880Hz %.0f", power660, power440, power880)
	}
}

// TestSpliceDoesNotRewriteTheFileItReplaces fails if a splice overwrites the
// file it replaces before freeing it. A segment deletion zeroes no whole
// audio file (Sami, 2026-11-12: "Do NOT zero entire files if people just want
// to delete certain segments",
// dispatch://INFRA-2961/ask/172349fa-0019-488c-9d3b-9d6b7af26a81). A
// descriptor held on the file reads its inode after the splice.
func TestSpliceDoesNotRewriteTheFileItReplaces(t *testing.T) {
	requireFFmpeg(t)
	path := filepath.Join(t.TempDir(), "tones.mp3")
	makeThreeToneMP3(t, path)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	held, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()

	if _, err := Splice(context.Background(), path, 3, 6); err != nil {
		t.Fatalf("Splice: %v", err)
	}
	got, err := io.ReadAll(io.NewSectionReader(held, 0, int64(len(original))+1))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Errorf("the splice rewrote the file it replaced (%d of %d bytes still the original)", commonPrefix(got, original), len(original))
	}
}

// TestSpliceFlushesItsOutputBeforeTheRename fails if Splice renames its
// output over the recording before flushing it: after a power loss the
// recording's name could hold an output whose data never reached the disk,
// and the live recording would be lost.
func TestSpliceFlushesItsOutputBeforeTheRename(t *testing.T) {
	requireFFmpeg(t)
	path := filepath.Join(t.TempDir(), "tones.mp3")
	makeThreeToneMP3(t, path)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	flush := syncFile
	t.Cleanup(func() { syncFile = flush })
	var flushed []string
	syncFile = func(output string) error {
		flushed = append(flushed, output)
		if !strings.HasPrefix(filepath.Base(output), ".splice-") {
			t.Errorf("flushed %s, want the splice output", output)
		}
		if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, original) {
			t.Errorf("the output was renamed over the recording before it was flushed (%v)", err)
		}
		return flush(output)
	}

	if _, err := Splice(context.Background(), path, 3, 6); err != nil {
		t.Fatalf("Splice: %v", err)
	}
	if len(flushed) != 1 {
		t.Fatalf("Splice flushed %v, want its output once", flushed)
	}
	if got, err := os.ReadFile(path); err != nil || bytes.Equal(got, original) {
		t.Fatalf("control: the recording was not spliced (%v)", err)
	}
}

func commonPrefix(a, b []byte) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

func TestSpliceIsAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recording.mp3")
	original := []byte("the original recording bytes")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatalf("write recording: %v", err)
	}

	previous := ffmpegBinary
	ffmpegBinary = filepath.Join(t.TempDir(), "missing-ffmpeg")
	t.Cleanup(func() { ffmpegBinary = previous })

	if _, err := Splice(context.Background(), path, 1, 2); err == nil {
		t.Fatal("Splice succeeded with a missing ffmpeg binary")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read recording after failed splice: %v", err)
	}
	if !bytes.Equal(after, original) {
		t.Fatalf("failed splice changed recording: got %q, want %q", after, original)
	}
}

func TestSurvivorsReturnsUniqueSegmentTokensLeftNearSplice(t *testing.T) {
	words := []Word{
		{Text: "before", Start: 8.5, End: 8.9},
		{Text: "secret", Start: 10.1, End: 10.5},
		{Text: "after", Start: 11, End: 11.3},
	}
	if got, want := Survivors(words, 10, "secret bridge", "before bridge", "after bridge"), []string{"secret"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("Survivors = %v, want %v", got, want)
	}
}

func TestSurvivorsAllowsSegmentTokensSharedWithNeighbours(t *testing.T) {
	words := []Word{{Text: "bridge", Start: 10.1, End: 10.4}}
	if got := Survivors(words, 10, "secret bridge", "before bridge", "after bridge"); len(got) != 0 {
		t.Fatalf("Survivors = %v, want no survivor for a neighbour word", got)
	}
}
