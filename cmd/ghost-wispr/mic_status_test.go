package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/server"
)

// TestAMicStreamErrorStatusNamesNoSession: the microphone stream fails when
// the write to the session's raw file fails (a full disk), and that error
// names the file, which is named for the session. The mic status is about the
// whole unit, so it is stored with no session_id and no delete removes it:
// it says the stream failed, not why. The log line keeps why.
func TestAMicStreamErrorStatusNamesNoSession(t *testing.T) {
	const id = "20261116090000"
	hub := server.NewHub()
	ch := hub.Subscribe()
	defer hub.Unsubscribe(ch)
	streamer := &fakeStreamer{errs: []error{errors.New("write raw pcm bytes: write /audio/" + id + ".pcm: no space left on device"), nil}}
	var logged []string
	streamMicWithRetry(context.Background(), newCaptureGate(nil, nil, nil), streamer, bytes.NewBuffer(nil), func(time.Duration) {}, func(format string, args ...any) {
		logged = append(logged, format)
		for _, a := range args {
			if err, ok := a.(error); ok {
				logged = append(logged, err.Error())
			}
		}
	}, hub)

	var statuses []string
	deadline := time.After(time.Second)
	for len(statuses) < 3 {
		select {
		case msg := <-ch:
			var payload map[string]any
			if err := json.Unmarshal(msg, &payload); err != nil {
				t.Fatal(err)
			}
			if payload["component"] == "mic" {
				statuses = append(statuses, string(msg))
			}
		case <-deadline:
			t.Fatalf("timeout waiting for mic statuses, got %v", statuses)
		}
	}
	if !strings.Contains(statuses[0], `"status":"error"`) {
		t.Fatalf("control: the first mic status is not the stream error: %v", statuses)
	}
	for _, status := range statuses {
		if strings.Contains(status, id) {
			t.Errorf("a mic status names session %s: %s", id, status)
		}
	}
	if !strings.Contains(strings.Join(logged, "\n"), id+".pcm") {
		t.Errorf("the log does not keep why the stream failed: %q", logged)
	}
}
