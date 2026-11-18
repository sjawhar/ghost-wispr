package audio

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/recording"
)

// pcmSeconds returns seconds of 16-bit mono PCM at rate, each sample its own
// index mod 32768, so a test can tell which part of the audio it holds.
func pcmSeconds(seconds float64, rate int, offset int) []byte {
	samples := int(seconds * float64(rate))
	pcm := make([]byte, 2*samples)
	for i := range samples {
		binary.LittleEndian.PutUint16(pcm[2*i:], uint16((offset+i)%32768))
	}
	return pcm
}

// TestAPrerollHoldsFiveSecondsAtTheRateTheMicOpenedAt fails if the audio
// buffered for a session's start (the preroll) is not the last 5 s at the
// rate the microphone opened at: it was sized for 16000 Hz, so a microphone
// opened at 48000 Hz led each session with 1.7 s. It also fails if audio at a
// rate before the change leads a session at the new one.
func TestAPrerollHoldsFiveSecondsAtTheRateTheMicOpenedAt(t *testing.T) {
	dir := t.TempDir()
	recorder := NewRecorder(dir)
	// Audio captured at 16000 Hz before the microphone opened again at 48000.
	if err := recorder.writePCM(pcmSeconds(1, 16000, 20000)); err != nil {
		t.Fatal(err)
	}
	if err := recorder.SetSampleRate(48000); err != nil {
		t.Fatal(err)
	}
	sixSeconds := pcmSeconds(6, 48000, 0)
	if err := recorder.writePCM(sixSeconds); err != nil {
		t.Fatal(err)
	}
	if err := recorder.StartSession("20261118100000"); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	recorder.mu.Lock()
	_ = recorder.rawFile.Sync()
	recorder.mu.Unlock()
	raw, err := os.ReadFile(recording.RawPath(dir, "20261118100000"))
	if err != nil {
		t.Fatal(err)
	}
	want := sixSeconds[len(sixSeconds)-5*48000*2:]
	if len(raw) != len(want) {
		t.Fatalf("the session opened with %d bytes of preroll (%.2f s at 48000 Hz), want %d (5 s)", len(raw), float64(len(raw))/96000, len(want))
	}
	if !bytes.Equal(raw, want) {
		t.Fatal("the preroll is not the last 5 s captured at 48000 Hz")
	}
}

// fakeRates is a SessionRates that keeps each session's rate in a map.
type fakeRates struct {
	mu    sync.Mutex
	rates map[string]int
}

func (f *fakeRates) SetSessionSampleRate(id string, rate int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rates == nil {
		f.rates = map[string]int{}
	}
	f.rates[id] = rate
	return nil
}

func (f *fakeRates) SessionSampleRate(id string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rates[id], nil
}

// durationOf returns the duration ffprobe reads from the audio file at path.
func durationOf(t *testing.T, path string) float64 {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "default=nw=1:nk=1", path).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", path, err)
	}
	seconds, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		t.Fatalf("ffprobe %s printed %q: %v", path, out, err)
	}
	return seconds
}

// TestARecoveredRawEncodesAtItsTrueDuration fails if a raw recording kept
// from a run that stopped inside its session (a power cut) is encoded at any
// rate but the one it was captured at: a kept .pcm stores no rate, and the
// recovery encoded it at the recorder's rate after the restart, 16000 Hz, so
// 2 s captured at 48000 Hz came out as 6 s. A raw from a build before the rate
// was recorded is encoded at 16000 Hz, the rate those builds captured at.
func TestARecoveredRawEncodesAtItsTrueDuration(t *testing.T) {
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed: %v", tool, err)
		}
	}
	dir := t.TempDir()
	rates := &fakeRates{}

	// The run that stopped: a session captured at 48000 Hz, never ended.
	stopped := NewRecorder(dir)
	stopped.UseSessionRates(rates)
	if err := stopped.SetSampleRate(48000); err != nil {
		t.Fatal(err)
	}
	const captured, older = "20261118100000", "20261118090000"
	if err := stopped.StartSession(captured); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	tone := make([]byte, 2*2*48000)
	for i := range 2 * 48000 {
		binary.LittleEndian.PutUint16(tone[2*i:], uint16(int16(8000*math.Sin(2*math.Pi*440*float64(i)/48000))))
	}
	if err := stopped.writePCM(tone); err != nil {
		t.Fatal(err)
	}
	stopped.mu.Lock()
	_ = stopped.rawFile.Close()
	stopped.mu.Unlock()
	// A raw an older build kept: 2 s at 16000 Hz, and no rate recorded.
	if err := os.WriteFile(recording.RawPath(dir, older), make([]byte, 2*2*16000), 0o644); err != nil {
		t.Fatal(err)
	}

	// The next start: the microphone has not opened yet.
	restarted := NewRecorder(dir)
	restarted.UseSessionRates(rates)
	for _, id := range []string{captured, older} {
		saved, err := restarted.SaveKept(context.Background(), id)
		if err != nil {
			t.Fatalf("SaveKept %s: %v", id, err)
		}
		if got := durationOf(t, saved); math.Abs(got-2) > 0.1 {
			t.Errorf("the recording recovered from %s lasts %.2f s, want 2 s, its true duration", filepath.Base(recording.RawPath(dir, id)), got)
		}
	}
}

// TestARecordingStartRefusesARateItCannotRecord fails if a session's raw
// recording starts though its rate could not be recorded: its recovery would
// then encode it at 16000 Hz whatever its true rate.
func TestARecordingStartRefusesARateItCannotRecord(t *testing.T) {
	dir := t.TempDir()
	recorder := NewRecorder(dir)
	recorder.UseSessionRates(refusingRates{})
	if err := recorder.StartSession("20261118100000"); err == nil {
		t.Fatal("StartSession succeeded though the session's rate could not be recorded")
	}
	if _, err := os.Lstat(recording.RawPath(dir, "20261118100000")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a raw recording was created with no rate recorded for it (%v)", err)
	}
}

type refusingRates struct{}

func (refusingRates) SetSessionSampleRate(string, int) error {
	return errors.New("database is locked")
}
func (refusingRates) SessionSampleRate(string) (int, error) { return 0, nil }

// writesOnce is a SessionRates whose first write works and the rest fail.
type writesOnce struct{ writes int }

func (w *writesOnce) SetSessionSampleRate(string, int) error {
	w.writes++
	if w.writes > 1 {
		return errors.New("database is locked")
	}
	return nil
}
func (w *writesOnce) SessionSampleRate(string) (int, error) { return 0, nil }

// TestARelabelThatCannotBeRecordedStillSavesAtTheNewRate fails if
// SetSampleRate hides a failed write of an open recording's new rate, or if
// that failure makes the session's end encode at the old rate: the recording
// in memory takes the new rate either way.
func TestARelabelThatCannotBeRecordedStillSavesAtTheNewRate(t *testing.T) {
	recorder := NewRecorder(t.TempDir())
	recorder.UseSessionRates(&writesOnce{})
	var encodedAt int
	recorder.encode = func(_ context.Context, _, _ string, rate int) (string, error) {
		encodedAt = rate
		return "", nil
	}
	if err := recorder.StartSession("20261118100000"); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := recorder.SetSampleRate(48000); err == nil {
		t.Fatal("SetSampleRate hid a failed write of the session's new rate")
	}
	if _, err := recorder.EndSession("20261118100000"); err != nil {
		t.Fatalf("EndSession: %v", err)
	}
	if encodedAt != 48000 {
		t.Fatalf("the session's end encoded at %d Hz, want 48000", encodedAt)
	}
}

// TestARawWithNoRateRecordedEncodesAtTheMicsRate fails if a raw kept by a
// build that recorded no rate (its row reads 0) is encoded at a fixed 16000 Hz
// rather than the rate this run's microphone opened at, as main encoded it:
// those builds had the same fallback to 48000, 44100 and 32000 Hz (ReviewPR78
// on #78, finding 2).
func TestARawWithNoRateRecordedEncodesAtTheMicsRate(t *testing.T) {
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed: %v", tool, err)
		}
	}
	dir := t.TempDir()
	const id = "20261118090000"
	if err := os.WriteFile(recording.RawPath(dir, id), make([]byte, 2*2*48000), 0o644); err != nil {
		t.Fatal(err)
	}
	recorder := NewRecorder(dir)
	recorder.UseSessionRates(&fakeRates{}) // the row reads 0
	if err := recorder.SetSampleRate(48000); err != nil {
		t.Fatal(err)
	}
	saved, err := recorder.SaveKept(context.Background(), id)
	if err != nil {
		t.Fatalf("SaveKept: %v", err)
	}
	if got := durationOf(t, saved); math.Abs(got-2) > 0.1 {
		t.Fatalf("a raw with no rate recorded, saved while the microphone is at 48000 Hz, lasts %.2f s, want 2 s", got)
	}
}
