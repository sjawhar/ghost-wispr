package audio

import (
	"bytes"
	"context"
	"encoding/binary"
	"log"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/recording"
)

// savingRecorder is a recorder whose saves succeed without an encoder.
func savingRecorder(t *testing.T) (string, *Recorder) {
	t.Helper()
	dir := t.TempDir()
	r := NewRecorder(dir)
	r.encode = func(context.Context, string, string, int) (string, error) { return "saved", nil }
	return dir, r
}

// openedWith starts session id and returns the labels of the seconds its
// raw recording opened with.
func openedWith(t *testing.T, dir string, r *Recorder, id string) []uint16 {
	t.Helper()
	if err := r.StartSession(id); err != nil {
		t.Fatalf("StartSession(%s): %v", id, err)
	}
	raw, err := os.ReadFile(recording.RawPath(dir, id))
	if err != nil {
		t.Fatal(err)
	}
	second := pcmBytesPerSecond(defaultSampleRate)
	if len(raw)%second != 0 {
		t.Fatalf("the recording opened with %d bytes, not whole seconds", len(raw))
	}
	var labels []uint16
	for at := 0; at < len(raw); at += second {
		label := binary.LittleEndian.Uint16(raw[at:])
		if !bytes.Equal(raw[at:at+second], holdSecond(label)) {
			t.Fatalf("second %d of the recording is not one captured second", at/second)
		}
		labels = append(labels, label)
	}
	return labels
}

// TestAHoldReachesNoAudioOfAnEndedSession fails if a hold for words heard
// late, after a session ended, takes any of that session's audio: the next
// recording would open with it, and a session a person deleted would come
// back in the next one's recording and in Drive. The recorder keeps audio
// for a session's start only while no recording is open.
func TestAHoldReachesNoAudioOfAnEndedSession(t *testing.T) {
	dir, r := savingRecorder(t)
	r.SetLookback(time.Minute)
	if err := r.StartSession("20261119080000"); err != nil {
		t.Fatal(err)
	}
	takeSeconds(t, r, secondsFrom(1, 10)...)
	if _, err := r.EndSession("20261119080000"); err != nil {
		t.Fatal(err)
	}
	takeSeconds(t, r, 20, 21)
	r.HoldPreroll(time.Minute)
	if got, want := openedWith(t, dir, r, "20261119080100"), []uint16{20, 21}; !slices.Equal(got, want) {
		t.Fatalf("the next recording opened with seconds %v, want %v: none of the ended session's", got, want)
	}
}

// TestAHoldDuringARecordingDoesNothing fails if a hold asked while a
// recording is open outlives it: the next recording would open with up to
// maxHeldSeconds instead of the usual preroll.
func TestAHoldDuringARecordingDoesNothing(t *testing.T) {
	dir, r := savingRecorder(t)
	r.SetLookback(time.Minute)
	if err := r.StartSession("20261119080000"); err != nil {
		t.Fatal(err)
	}
	r.HoldPreroll(time.Second)
	if _, err := r.EndSession("20261119080000"); err != nil {
		t.Fatal(err)
	}
	takeSeconds(t, r, secondsFrom(1, 10)...)
	if got, want := openedWith(t, dir, r, "20261119080100"), secondsFrom(6, 10); !slices.Equal(got, want) {
		t.Fatalf("the next recording opened with seconds %v, want %v, the usual preroll", got, want)
	}
}

// TestARepeatedHoldForTheSameWordsKeepsTheStart fails if each later result
// for the same first words, which reports them further back by the audio
// taken since, moves the recording's start back by that much: with interim
// results every second, the start drifted back by the utterance's length.
func TestARepeatedHoldForTheSameWordsKeepsTheStart(t *testing.T) {
	dir, r := savingRecorder(t)
	r.SetLookback(time.Minute)
	takeSeconds(t, r, secondsFrom(1, 20)...)
	r.HoldPreroll(12 * time.Second) // the first word began at second 9
	takeSeconds(t, r, 21, 22)
	r.HoldPreroll(14 * time.Second) // the same first word, 14 s ago now
	if got, want := openedWith(t, dir, r, "20261119080000"), secondsFrom(4, 22); !slices.Equal(got, want) {
		t.Fatalf("the recording opened with seconds %v, want %v", got, want)
	}
}

// TestASampleRateChangeDropsTheHold fails if audio a hold kept at the old
// rate, or silence in its place, leads a recording at the new one.
func TestASampleRateChangeDropsTheHold(t *testing.T) {
	dir := t.TempDir()
	r := NewRecorder(dir)
	r.SetLookback(time.Minute)
	takeSeconds(t, r, secondsFrom(1, 10)...)
	r.HoldPreroll(5 * time.Second)
	takeSeconds(t, r, 11, 12)
	if err := r.SetSampleRate(48000); err != nil {
		t.Fatal(err)
	}
	second := make([]byte, pcmBytesPerSecond(48000))
	for i := 0; i < len(second); i += 2 {
		binary.LittleEndian.PutUint16(second[i:], 50)
	}
	if err := r.writePCM(second); err != nil {
		t.Fatal(err)
	}
	if err := r.StartSession("20261119080000"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(recording.RawPath(dir, "20261119080000"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, second) {
		t.Fatalf("the recording opened with %d bytes, want only the %d of the second at 48000 Hz", len(raw), len(second))
	}
}

// TestAShorterLookbackDuringAHoldAddsNoSilence fails if a hold that reaches
// further back than a shortened history still holds leads the recording with
// silence in place of the audio that went.
func TestAShorterLookbackDuringAHoldAddsNoSilence(t *testing.T) {
	dir, r := savingRecorder(t)
	r.SetLookback(time.Minute)
	takeSeconds(t, r, secondsFrom(1, 20)...)
	r.HoldPreroll(12 * time.Second)
	r.SetLookback(5 * time.Second)
	if got, want := openedWith(t, dir, r, "20261119080000"), secondsFrom(11, 20); !slices.Equal(got, want) {
		t.Fatalf("the recording opened with seconds %v, want %v", got, want)
	}
}

// TestAHoldThatDropsItsOldestAudioWarns fails if a hold at maxHeldSeconds
// drops its oldest audio, which can be the first words, without a warning in
// the log, once per hold. The warning names no words.
func TestAHoldThatDropsItsOldestAudioWarns(t *testing.T) {
	var logged bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(previous) })
	_, r := savingRecorder(t)
	takeSeconds(t, r, 1)
	r.HoldPreroll(0)
	takeSeconds(t, r, secondsFrom(2, maxHeldSeconds+5)...)
	if n := strings.Count(logged.String(), "warning: "); n != 1 {
		t.Fatalf("the log holds %d warnings, want 1 for a hold past %d s:\n%s", n, maxHeldSeconds, logged.String())
	}
	if !strings.Contains(logged.String(), "first words") {
		t.Fatalf("the warning does not say the first words can be lost: %s", logged.String())
	}
}
