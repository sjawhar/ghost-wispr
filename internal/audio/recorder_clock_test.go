package audio

import (
	"testing"
	"time"
)

// TestPendingIsWhatTheNextRecordingOpensWith fails if Pending does not say
// how much audio the next session's recording would open with now: the last
// prerollSeconds, or all that a hold keeps, and nothing while a recording is
// open. A session's start time is set from it, so that a refined segment's
// time, its offset into the recording added to the session's start, is the
// time it was spoken.
func TestPendingIsWhatTheNextRecordingOpensWith(t *testing.T) {
	_, r := savingRecorder(t)
	r.SetLookback(time.Minute)
	if got := r.Pending(); got != 0 {
		t.Fatalf("Pending() with no audio = %v, want 0", got)
	}
	takeSeconds(t, r, secondsFrom(1, 10)...)
	if got := r.Pending(); got != prerollSeconds*time.Second {
		t.Fatalf("Pending() with no hold = %v, want the preroll", got)
	}
	r.HoldPreroll(3 * time.Second)
	takeSeconds(t, r, 11, 12)
	if got, want := r.Pending(), 10*time.Second; got != want {
		t.Fatalf("Pending() with a hold = %v, want %v: 8 s before the hold and the 2 s it took", got, want)
	}
	if err := r.StartSession("20261119080000"); err != nil {
		t.Fatal(err)
	}
	if got := r.Pending(); got != 0 {
		t.Fatalf("Pending() while a recording is open = %v, want 0", got)
	}
}

// TestSinceCloseCountsTheAudioAfterARecordingCloses fails if SinceClose does
// not say how much audio was taken after the last recording closed, which is
// how the session manager tells words spoken before an ending session's
// recording closed (they are in it) from words spoken after (they open the
// next session), or does not say when a recording is open.
func TestSinceCloseCountsTheAudioAfterARecordingCloses(t *testing.T) {
	_, r := savingRecorder(t)
	r.SetLookback(10 * time.Second)
	if err := r.StartSession("20261119080000"); err != nil {
		t.Fatal(err)
	}
	takeSeconds(t, r, 1, 2)
	if _, closed := r.SinceClose(); closed {
		t.Fatal("SinceClose() says no recording is open while one is")
	}
	if _, err := r.EndSession("20261119080000"); err != nil {
		t.Fatal(err)
	}
	takeSeconds(t, r, secondsFrom(3, 22)...)
	if got, closed := r.SinceClose(); got != 20*time.Second || !closed {
		t.Fatalf("SinceClose() = %v, %t; want 20s, true: more than history keeps", got, closed)
	}
}
