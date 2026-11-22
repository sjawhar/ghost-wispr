package audio

import (
	"slices"
	"testing"
	"time"
)

// TestAHoldPastItsBoundTakesNoHistoryBack fails if a later hold that reaches
// further back takes audio from history in front of a hold already at
// maxHeldSeconds. Once a hold has dropped its oldest audio (writePCM), history
// no longer joins what it holds: the recording opened with history, a gap
// where the dropped audio was, then the audio held, and ran past
// maxHeldSeconds. The history here keeps 245 s, under maxHeldSeconds, so the
// hold's limit is maxHeldSeconds.
func TestAHoldPastItsBoundTakesNoHistoryBack(t *testing.T) {
	dir := t.TempDir()
	r := NewRecorder(dir)
	r.SetLookback(4 * time.Minute)
	takeSeconds(t, r, secondsFrom(1, 20)...)
	r.HoldPreroll(time.Second)
	takeSeconds(t, r, secondsFrom(21, maxHeldSeconds+40)...)
	r.HoldPreroll((maxHeldSeconds + 30) * time.Second)
	got := startedWith(t, dir, r)
	if want := secondsFrom(41, maxHeldSeconds+40); !slices.Equal(got, want) {
		t.Fatalf("the recording opened with %d seconds: %v; want the last %d, seconds 41 to %d", len(got), got, maxHeldSeconds, maxHeldSeconds+40)
	}
}

// TestAHoldThatReachesFurtherKeepsAtMostMaxHeldSeconds fails if a later hold
// that reaches further back makes a hold whose held audio is under
// maxHeldSeconds lead the next recording with more than maxHeldSeconds in
// all: only writePCM bounded it, so a session started before the next write
// opened with all of it.
func TestAHoldThatReachesFurtherKeepsAtMostMaxHeldSeconds(t *testing.T) {
	dir := t.TempDir()
	r := NewRecorder(dir)
	r.SetLookback(4 * time.Minute) // history keeps 245 s: the limit is maxHeldSeconds
	takeSeconds(t, r, secondsFrom(1, 100)...)
	r.HoldPreroll(50 * time.Second)
	takeSeconds(t, r, secondsFrom(101, 350)...)
	r.HoldPreroll(320 * time.Second)
	got := startedWith(t, dir, r)
	if want := secondsFrom(51, 350); !slices.Equal(got, want) {
		t.Fatalf("the recording opened with %d seconds, %s; want the last %d, seconds 51 to 350", len(got), span(got), maxHeldSeconds)
	}
}

// TestAHoldReachesAsFarBackAsTheHistory fails if a hold stops at
// maxHeldSeconds while the history keeps more: a live buffer long enough
// (deepgram_buffer_size) makes Deepgram hear words more than 300 s late after
// an outage, and the session sizes the history to reach them
// (session.Manager.SetLiveStream), but the recording opened without them. With
// 605 s of history, a hold for words 390 s ago opens the recording 395 s back,
// and keeps the audio taken after it too.
func TestAHoldReachesAsFarBackAsTheHistory(t *testing.T) {
	dir := t.TempDir()
	r := NewRecorder(dir)
	r.SetLookback(10 * time.Minute)
	takeSeconds(t, r, secondsFrom(1, 400)...)
	r.HoldPreroll(390 * time.Second)
	takeSeconds(t, r, secondsFrom(401, 450)...)
	got := startedWith(t, dir, r)
	if want := secondsFrom(6, 450); !slices.Equal(got, want) {
		t.Fatalf("the recording opened with %d seconds, %s; want seconds 6 to 450, the 395 s before the words and all since", len(got), span(got))
	}
}
