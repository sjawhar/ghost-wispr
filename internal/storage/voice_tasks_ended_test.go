package storage

import (
	"slices"
	"testing"
	"time"
)

// TestCancelWorkingVoiceTasksOfEndedSessions: the working questions of every
// session no longer recording are cancelled, and their sessions named; a
// recording session's, and an answered question, stay as they are.
func TestCancelWorkingVoiceTasksOfEndedSessions(t *testing.T) {
	store := newTestSQLiteStore(t)
	at := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	for _, id := range []string{"ended", "live", "discarded"} {
		if err := store.CreateSession(id, at); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.EndSession("ended", at.Add(time.Hour), ""); err != nil {
		t.Fatal(err)
	}
	if err := store.DiscardSession("discarded"); err != nil {
		t.Fatal(err)
	}
	for _, task := range []VoiceTask{
		{ID: "e1", SessionID: "ended", Holder: "session:agent-7"}, {ID: "e2", SessionID: "ended"},
		{ID: "l1", SessionID: "live"}, {ID: "d1", SessionID: "discarded"}, {ID: "e3", SessionID: "ended"},
	} {
		task.AskedAt = at
		if err := store.CreateVoiceTask(&task); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.AnswerVoiceTask("e3", "Done."); err != nil {
		t.Fatal(err)
	}
	cancelled, err := store.CancelWorkingVoiceTasksOfEndedSessions()
	if err != nil {
		t.Fatal(err)
	}
	if ids := taskIDs(cancelled); !slices.Equal(ids, []string{"d1", "e1", "e2"}) || cancelled[1].Holder != "session:agent-7" || cancelled[1].State != VoiceTaskCancelled {
		t.Fatalf("cancelled = %+v, want d1, e1 (with its holder) and e2, cancelled", cancelled)
	}
	for id, want := range map[string]VoiceTaskState{"e1": VoiceTaskCancelled, "e2": VoiceTaskCancelled, "d1": VoiceTaskCancelled, "l1": VoiceTaskWorking, "e3": VoiceTaskDone} {
		if task, err := store.VoiceTask(id); err != nil || task.State != want {
			t.Errorf("%s = %+v, %v; want %s", id, task, err, want)
		}
	}
	if again, err := store.CancelWorkingVoiceTasksOfEndedSessions(); err != nil || len(again) != 0 {
		t.Fatalf("a second pass = %q, %v; want none", again, err)
	}
}

// TestCancelAllWorkingVoiceTasks: every working question, of any session, is
// cancelled and returned, by session; an answered one stays done.
func TestCancelAllWorkingVoiceTasks(t *testing.T) {
	store := newTestSQLiteStore(t)
	at := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	for _, id := range []string{"a", "b"} {
		if err := store.CreateSession(id, at); err != nil {
			t.Fatal(err)
		}
	}
	for _, task := range []VoiceTask{{ID: "a1", SessionID: "a", Holder: "session:agent-7"}, {ID: "b1", SessionID: "b"}, {ID: "b2", SessionID: "b"}} {
		task.AskedAt = at
		if err := store.CreateVoiceTask(&task); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.AnswerVoiceTask("b2", "Done."); err != nil {
		t.Fatal(err)
	}
	cancelled, err := store.CancelAllWorkingVoiceTasks()
	if ids := taskIDs(cancelled); err != nil || !slices.Equal(ids, []string{"a1", "b1"}) || cancelled[0].Holder != "session:agent-7" {
		t.Fatalf("cancelled = %+v, %v; want a1 (with its holder) and b1", cancelled, err)
	}
	for id, want := range map[string]VoiceTaskState{"a1": VoiceTaskCancelled, "b1": VoiceTaskCancelled, "b2": VoiceTaskDone} {
		if task, err := store.VoiceTask(id); err != nil || task.State != want {
			t.Errorf("%s = %+v, %v; want %s", id, task, err, want)
		}
	}
}

func taskIDs(tasks []VoiceTask) []string {
	ids := make([]string, len(tasks))
	for i := range tasks {
		ids[i] = tasks[i].ID
	}
	return ids
}
