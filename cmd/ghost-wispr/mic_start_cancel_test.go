package main

import (
	"context"
	"errors"
	"testing"
)

// TestAMicStartOpensNothingOnceAPauseOrShutdownCame fails if a microphone
// start whose scan a pause or shutdown overtook goes on to open a device.
// Shutdown waits 5 s for the mic loops, then turns the light off; a scan that
// outlasts that wait would open a microphone nothing closes, with the light
// off (ReviewPR77 on #77, round 3). The control: neither came, and the start
// opens.
func TestAMicStartOpensNothingOnceAPauseOrShutdownCame(t *testing.T) {
	capture := newCaptureGate(nil, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	if err := micStartCancelled(ctx, capture); err != nil {
		t.Fatalf("control: with neither a pause nor shutdown, the start was cancelled: %v", err)
	}

	cancel()
	if err := micStartCancelled(ctx, capture); !errors.Is(err, context.Canceled) {
		t.Fatalf("a start after shutdown began = %v, want it cancelled by the shutdown", err)
	}

	capture.Pause()
	if err := micStartCancelled(context.Background(), capture); !errors.Is(err, errMicStartPaused) {
		t.Fatalf("a start after a pause = %v, want errMicStartPaused", err)
	}
}
