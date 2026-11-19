package audio

import (
	"encoding/binary"
	"fmt"
	"slices"
	"testing"
	"time"
)

// holdSecond returns one second of audio at the default rate whose samples
// all hold label, so a test can tell which second a recording holds.
func holdSecond(label uint16) []byte {
	pcm := make([]byte, pcmBytesPerSecond(defaultSampleRate))
	for i := 0; i < len(pcm); i += 2 {
		binary.LittleEndian.PutUint16(pcm[i:], label)
	}
	return pcm
}

func takeSeconds(t *testing.T, r *Recorder, labels ...uint16) {
	t.Helper()
	for _, label := range labels {
		if err := r.writePCM(holdSecond(label)); err != nil {
			t.Fatal(err)
		}
	}
}

func secondsFrom(first, last uint16) []uint16 {
	var labels []uint16
	for label := first; label <= last; label++ {
		labels = append(labels, label)
	}
	return labels
}

// startedWith starts a session and returns the labels of the seconds its
// raw recording opened with.
func startedWith(t *testing.T, dir string, r *Recorder) []uint16 {
	t.Helper()
	return openedWith(t, dir, r, "20261119060000")
}

// span names the first and the last of labels, or none.
func span(labels []uint16) string {
	if len(labels) == 0 {
		return "none"
	}
	return fmt.Sprintf("%d to %d", labels[0], labels[len(labels)-1])
}

// TestAHoldReachesBackToWordsHeardLate fails if a hold (HoldPreroll) for
// words captured long before Deepgram heard them does not start the
// recording prerollSeconds before them, within the lookback, and keep all
// the audio after. A later hold that reaches less far must not move the
// start forward.
func TestAHoldReachesBackToWordsHeardLate(t *testing.T) {
	dir := t.TempDir()
	r := NewRecorder(dir)
	r.SetLookback(30 * time.Second)
	takeSeconds(t, r, secondsFrom(1, 20)...)
	r.HoldPreroll(12 * time.Second) // the first words began at second 9
	takeSeconds(t, r, 21, 22)
	r.HoldPreroll(time.Second)
	if got, want := startedWith(t, dir, r), secondsFrom(4, 22); !slices.Equal(got, want) {
		t.Fatalf("the recording opened with seconds %v, want %v: from 5 s before the first words", got, want)
	}
}

// TestAHoldReachesNoFurtherThanTheLookback fails if the recorder keeps more
// audio from before a session than the preroll and the lookback, however far
// back a hold asks for.
func TestAHoldReachesNoFurtherThanTheLookback(t *testing.T) {
	dir := t.TempDir()
	r := NewRecorder(dir)
	r.SetLookback(10 * time.Second)
	takeSeconds(t, r, secondsFrom(1, 30)...)
	r.HoldPreroll(25 * time.Second)
	if got, want := startedWith(t, dir, r), secondsFrom(16, 30); !slices.Equal(got, want) {
		t.Fatalf("the recording opened with seconds %v, want %v: the last 15 s", got, want)
	}
}

// TestAResumeDropsTheAudioAHoldKept fails if audio a hold kept from before a
// capture pause opens the first session after the resume: a resume drops it
// (DiscardPreroll) with the rest of the audio from before the pause.
func TestAResumeDropsTheAudioAHoldKept(t *testing.T) {
	dir := t.TempDir()
	r := NewRecorder(dir)
	r.SetLookback(30 * time.Second)
	takeSeconds(t, r, 1, 2, 3)
	r.HoldPreroll(2 * time.Second)
	takeSeconds(t, r, 4, 5)
	r.DiscardPreroll()
	takeSeconds(t, r, 50)
	if got, want := startedWith(t, dir, r), []uint16{50}; !slices.Equal(got, want) {
		t.Fatalf("the recording opened with seconds %v, want %v: only audio from after the resume", got, want)
	}
}

// TestAReleasedHoldLeadsWithOnlyThePreroll fails if words Deepgram took back
// (ReleasePreroll) still lead the next session with the audio held for them:
// it opens with the last prerollSeconds, as with no hold.
func TestAReleasedHoldLeadsWithOnlyThePreroll(t *testing.T) {
	dir := t.TempDir()
	r := NewRecorder(dir)
	r.SetLookback(30 * time.Second)
	takeSeconds(t, r, 1, 2, 3)
	r.HoldPreroll(time.Second)
	takeSeconds(t, r, secondsFrom(4, 12)...)
	r.ReleasePreroll()
	takeSeconds(t, r, 13)
	if got, want := startedWith(t, dir, r), secondsFrom(9, 13); !slices.Equal(got, want) {
		t.Fatalf("the recording opened with seconds %v, want %v: the last 5 s", got, want)
	}
}

// TestAHoldKeepsAtMostMaxHeldSeconds fails if a hold that no session ends
// keeps more than maxHeldSeconds of audio.
func TestAHoldKeepsAtMostMaxHeldSeconds(t *testing.T) {
	dir := t.TempDir()
	r := NewRecorder(dir)
	r.SetLookback(30 * time.Second)
	takeSeconds(t, r, secondsFrom(1, 10)...)
	r.HoldPreroll(5 * time.Second)
	takeSeconds(t, r, secondsFrom(11, maxHeldSeconds+20)...)
	if got, want := startedWith(t, dir, r), secondsFrom(21, maxHeldSeconds+20); !slices.Equal(got, want) {
		t.Fatalf("the recording opened with %d seconds, %s; want the last %d", len(got), span(got), maxHeldSeconds)
	}
}
