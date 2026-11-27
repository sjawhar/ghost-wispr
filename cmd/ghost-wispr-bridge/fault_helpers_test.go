package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// faultMeetURL is the Meet link of every test run spec.
const faultMeetURL = "https://meet.google.com/abc-defg-hij"

// faultStart is the bridge's start in every fault test.
var faultStart = time.Date(2026, 12, 1, 16, 0, 0, 0, time.UTC)

// faultSpec is a run spec as the live-check workflow writes it (plan C8;
// its fault as a list, contract A4), ending end and naming faults.
func faultSpec(end time.Time, faults ...string) map[string]any {
	if faults == nil {
		faults = []string{}
	}
	return map[string]any{
		"meet_url":      faultMeetURL,
		"end_time":      end.Format(time.RFC3339),
		"fault":         faults,
		"fault_after_s": 600,
		"worker": map[string]any{
			"log_level": "ERROR", "pulse_48k": true, "celery_pool": "threads",
			"kill_chrome_after_s": 0, "tls_probe": false,
		},
		"upkeep": map[string]any{"sentinel": []string{}, "skip_delete": false},
	}
}

// faultWriteSpec writes spec as JSON to a file of the test's own and gives
// its path.
func faultWriteSpec(t *testing.T, spec map[string]any) string {
	t.Helper()
	b, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal the run spec: %v", err)
	}
	return faultWriteFile(t, string(b))
}

// faultWriteFile writes body to a file of the test's own and gives its path.
func faultWriteFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "spec.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write the run spec: %v", err)
	}
	return path
}

// faultBridge is main's path, startBridge, running in a goroutine.
type faultBridge struct {
	cancel context.CancelFunc
	done   chan error
}

// faultStartBridge runs startBridge on s and deps until the test stops it.
// The test reads deps only after stop: startBridge changes them.
func faultStartBridge(t *testing.T, s *settings, deps *bridgeDeps) *faultBridge {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	b := &faultBridge{cancel: cancel, done: make(chan error, 1)}
	go func() { b.done <- startBridge(ctx, s, deps) }()
	return b
}

// stillRunning fails the test when startBridge returned before its context
// ended.
func (b *faultBridge) stillRunning(t *testing.T) {
	t.Helper()
	select {
	case err := <-b.done:
		t.Fatalf("startBridge returned %v before its context ended", err)
	case <-time.After(50 * time.Millisecond):
	}
}

// stop ends startBridge's context and gives its error.
func (b *faultBridge) stop(t *testing.T) error {
	t.Helper()
	b.cancel()
	select {
	case err := <-b.done:
		return err
	case <-time.After(clockWaitLimit):
		t.Fatal("startBridge did not return after its context ended")
		return nil
	}
}
