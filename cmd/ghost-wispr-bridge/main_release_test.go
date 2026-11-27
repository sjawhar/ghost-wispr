//go:build !callfault

package main

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// startBridge is main's path after the wiring: applyFaults, then runBridge
// until ctx ends. In the release build applyFaults stops nothing; the
// callfault build's refusals are TestAnExpiredRunSpecMakesNoBotAndNoLink's.
func TestStartBridgeRunsTheBridgeUntilTheContextEnds(t *testing.T) {
	clk := newFakeClock(botT0)
	api := newFakeAttendee(clk)
	logs := &botLogs{}
	s := botSettingsForTest()
	deps := bridgeDeps{API: api, Calendar: newFakeCalendar(clk), Clock: clk, Log: newLogger(logs, "bridge", slog.LevelDebug), Sockets: &botSockets{}}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- startBridge(ctx, &s, &deps) }()
	clockWaitFor(t, func() bool { return len(api.callsOf("Bots")) == 1 }, "the bridge's read of Attendee's bots")
	select {
	case err := <-done:
		t.Fatalf("startBridge returned %v before its context ended", err)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("startBridge: %v", err)
	}
	if !strings.Contains(logs.String(), "BRIDGE-START") {
		t.Fatalf("no BRIDGE-START line: %s", logs.String())
	}
}
