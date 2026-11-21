package main

import (
	"context"
	"time"
)

// shutdownWait bounds the part of the shutdown that waits for the store's
// work, and the store's close (shutdownSteps.run).
const shutdownWait = 30 * time.Second

// shutdownSteps are main's shutdown. run runs them in this order:
//  1. The voice stops: its session, its speech and the Concierge's calls.
//  2. No opening opens the capture gate, whatever its red check finds
//     (captureGate.Stop).
//  3. The app's context ends, which ends the mic loops and the paused
//     listener.
//  4. No session_started send starts.
//  5. The mic loops are joined. Only then does the light go off: its stop
//     refuses any later red confirmation.
//  6. Within the shutdown's deadline: HTTP stops serving, the store's work
//     is stopped and joined, the devices and the Concierge stop, and last the
//     store closes, never under a statement still running (storage).
type shutdownSteps struct {
	closeVoice         func()
	stopCapture        func()
	stopApp            func()
	stopStartPublishes func()
	joinMicLoops       func() bool // reports whether every mic loop ended
	lightOff           func()
	stopServing        func(ctx context.Context)
	stopStoreWork      func(ctx context.Context)
	stopDevices        func(ctx context.Context, micLoopsEnded bool)
	closeStore         func(ctx context.Context)
}

// run runs the steps in their order, the last four within wait.
func (s *shutdownSteps) run(wait time.Duration) {
	s.closeVoice()
	s.stopCapture()
	s.stopApp()
	s.stopStartPublishes()
	micLoopsEnded := s.joinMicLoops()
	s.lightOff()
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	s.stopServing(ctx)
	s.stopStoreWork(ctx)
	s.stopDevices(ctx, micLoopsEnded)
	s.closeStore(ctx)
}
