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
// maxHeldSeconds. A live buffer longer than maxHeldSeconds
// (deepgram_buffer_size) makes Deepgram hear words that late after an outage.
func TestAHoldPastItsBoundTakesNoHistoryBack(t *testing.T) {
	dir := t.TempDir()
	r := NewRecorder(dir)
	r.SetLookback(10 * time.Minute)
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
	r.SetLookback(10 * time.Minute)
	takeSeconds(t, r, secondsFrom(1, 100)...)
	r.HoldPreroll(50 * time.Second)
	takeSeconds(t, r, secondsFrom(101, 350)...)
	r.HoldPreroll(320 * time.Second)
	got := startedWith(t, dir, r)
	if want := secondsFrom(51, 350); !slices.Equal(got, want) {
		t.Fatalf("the recording opened with %d seconds, %d to %d; want the last %d, seconds 51 to 350", len(got), got[0], got[len(got)-1], maxHeldSeconds)
	}
}
