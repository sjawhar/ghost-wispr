package transcribe

import (
	"context"
	"io"
	"testing"
	"time"
)

// TestForgottenDropsKeepTheirPlaceAcrossAReconnect fails if a reconnect that
// forgets the send queue's drops before the new socket's first byte
// (keepDropsAfterLocked) makes the new socket's points count a drop twice,
// or not at all: no other test reaches the running drop sums less dropsBase
// across a reconnect (Sol's review of #133 at 51143e4, which found each of
// three faults passing every other test: the dropsBase subtraction left out,
// the forgottenDrops increment left out, the dropsBase update left out).
//
// Capture order, on the count Captured gives:
//
//	A 0-1 s sent | D1 1-2 s dropped | C1 2-3 s buffered | D2 3-4 s dropped |
//	C2 4-5 s buffered | E 5-6 s sent after the reconnect.
//
// The new socket starts at C1, so D1 is forgotten and D2, between C1 and C2,
// is kept.
func TestForgottenDropsKeepTheirPlaceAcrossAReconnect(t *testing.T) {
	rc := NewResilientClient(t.Context(), func(context.Context) (io.WriteCloser, error) { return &mockWriter{}, nil }, ResilientConfig{SampleRate: 16000}, nil, nil)
	rc.reconnect(0)
	_, _ = rc.Write(make([]byte, secondOfAudio)) // A, sent
	rc.sendQueueDropped(secondOfAudio)           // D1
	rc.Mu.Lock()
	rc.State = StateDisconnected
	rc.Mu.Unlock()
	_, _ = rc.Write(make([]byte, secondOfAudio)) // C1, buffered
	rc.sendQueueDropped(secondOfAudio)           // D2
	_, _ = rc.Write(make([]byte, secondOfAudio)) // C2, buffered
	rc.reconnect(0)                              // the new stream: C1 0-1 s, C2 1-2 s
	if !rc.IsConnected() {
		t.Fatal("control: the reconnect did not drain and connect")
	}
	lo := time.Now()
	_, _ = rc.Write(make([]byte, secondOfAudio)) // E: stream 2-3 s
	hi := time.Now()

	if got, want := rc.Captured(), 6*time.Second; got != want {
		t.Fatalf("Captured() = %v, want %v", got, want)
	}
	if got, want := rc.CapturedBefore(0), 2*time.Second; got != want {
		t.Fatalf("CapturedBefore(0) = %v, want %v: the new stream starts at C1, after A and D1", got, want)
	}
	if got, want := rc.CapturedBefore(1500*time.Millisecond), 4500*time.Millisecond; got != want {
		t.Fatalf("CapturedBefore(1.5s) = %v, want %v: the middle of C2, after D2", got, want)
	}
	if got, want := rc.CapturedSince(1500*time.Millisecond), 1500*time.Millisecond; got != want {
		t.Fatalf("CapturedSince(1.5s) = %v, want %v: the rest of C2 and E", got, want)
	}
	at, _, ok := rc.CapturedAt(2500 * time.Millisecond)
	if !ok || at.Before(lo.Add(-500*time.Millisecond)) || at.After(hi.Add(-500*time.Millisecond)) {
		t.Fatalf("CapturedAt(2.5s) = %v, %v; want E's write time less 0.5 s (%v to %v)", at.Format(time.StampMicro), ok, lo.Add(-500*time.Millisecond).Format(time.StampMicro), hi.Add(-500*time.Millisecond).Format(time.StampMicro))
	}
}
