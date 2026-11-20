package main

import (
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// TestARecoveredSessionsQuestionsAreCancelled fails if a session the start-up
// recovery ends keeps a question working: its late answer would then be
// announced in a later meeting.
func TestARecoveredSessionsQuestionsAreCancelled(t *testing.T) {
	store := conciergeStore(t)
	at := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	for _, id := range []string{"recovered", "other"} {
		if err := store.CreateSession(id, at); err != nil {
			t.Fatal(err)
		}
		if err := store.CreateVoiceTask(&storage.VoiceTask{ID: "task-" + id, SessionID: id, AskedAt: at, IssueKey: "ABC-1", Holder: "session:agent"}); err != nil {
			t.Fatal(err)
		}
	}
	cancelRecoveredQuestions(store, []string{"recovered"}, t.Logf)
	for id, want := range map[string]storage.VoiceTaskState{"recovered": storage.VoiceTaskCancelled, "other": storage.VoiceTaskWorking} {
		task, err := store.VoiceTask("task-" + id)
		if err != nil {
			t.Fatal(err)
		}
		if task.State != want {
			t.Errorf("session %s's question is %q, want %q", id, task.State, want)
		}
	}
}
