package session

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// TestAManualSessionsTitleHintIsNotLogged: a manual session's title hint is
// the title it starts with, and a title names the conversation's topic. The
// app's log (journald, and GET /api/logs) keeps a session's id, never its
// words: starting a session with a title hint logs the start without it.
func TestAManualSessionsTitleHintIsNotLogged(t *testing.T) {
	const hint = "Purple elephant seventeen budget"
	var logs bytes.Buffer
	manager := NewManager(newStoreMock(), nil, nil, nil, NewDetector(time.Hour), 0, slog.New(slog.NewTextHandler(&logs, nil)))
	id, err := manager.ManualStartSession(context.Background(), hint)
	if err != nil {
		t.Fatalf("ManualStartSession: %v", err)
	}
	if !strings.Contains(logs.String(), id) {
		t.Fatalf("control: the log does not name session %s, so it shows nothing: %s", id, logs.String())
	}
	for _, word := range strings.Fields(hint) {
		if strings.Contains(strings.ToLower(logs.String()), strings.ToLower(word)) {
			t.Errorf("the log holds %q of the title hint: %s", word, logs.String())
		}
	}
}
