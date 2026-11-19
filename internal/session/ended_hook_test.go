package session

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// TestTheSessionEndedHookHearsEveryEnd fails if a session that ends, kept or
// discarded as too short, does not reach the session-ended hook with its id:
// the meeting voice cancels the questions an ended meeting still waits on.
func TestTheSessionEndedHookHearsEveryEnd(t *testing.T) {
	for name, minSegments := range map[string]int{"kept": 0, "discarded as too short": 2} {
		t.Run(name, func(t *testing.T) {
			store := newStoreMock()
			manager := NewManager(store, nil, nil, nil, NewDetector(time.Hour), minSegments)
			ended := make(chan string, 1)
			manager.SetSessionEndedHook(func(sessionID string) { ended <- sessionID })

			if err := manager.ensureSessionStarted(time.Now().UTC()); err != nil {
				t.Fatalf("start session: %v", err)
			}
			sessionID := manager.currentSession()
			if err := store.AppendSegment(sessionID, transcribe.Segment{Text: "one segment"}); err != nil {
				t.Fatalf("AppendSegment: %v", err)
			}
			if err := manager.endCurrentSession(context.Background()); err != nil {
				t.Fatalf("end session: %v", err)
			}
			select {
			case got := <-ended:
				if got != sessionID {
					t.Fatalf("the hook heard %q, want %q", got, sessionID)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("the session-ended hook heard nothing")
			}
		})
	}
}
