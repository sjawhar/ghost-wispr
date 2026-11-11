package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/summary"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

type r4BatchHub struct{ ready []string }

func (h *r4BatchHub) BroadcastSummaryReady(sessionID, _, _, _, _ string) {
	h.ready = append(h.ready, sessionID)
}

func r4BatchStore(t *testing.T, ids ...string) (*storage.SQLiteStore, string, string, map[string]int) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "batch.db")
	store, err := storage.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	generations := make(map[string]int, len(ids))
	for i, id := range ids {
		if err := store.CreateSession(id, started.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if err := store.AppendSegment(id, transcribe.Segment{Text: "before private " + id, StartTime: 0, EndTime: 1, Timestamp: started}); err != nil {
			t.Fatal(err)
		}
		if err := store.EndSession(id, started.Add(time.Minute), "recording.mp3"); err != nil {
			t.Fatal(err)
		}
		if err := store.Canonicalize(id); err != nil {
			t.Fatal(err)
		}
		generation, err := store.DeletionGeneration(id)
		if err != nil {
			t.Fatal(err)
		}
		generations[id] = generation
		if err := store.StartSummary(id); err != nil {
			t.Fatal(err)
		}
	}
	batchName := "batches/r4"
	if err := store.RecordSummaryBatch(batchName, generations); err != nil {
		t.Fatal(err)
	}
	return store, path, batchName, generations
}

func r4BatchState(t *testing.T, store *storage.SQLiteStore, batchName, sessionID string) string {
	t.Helper()
	var state string
	if err := store.DB().QueryRow(`SELECT state FROM summary_batches WHERE batch_name = ? AND session_id = ?`, batchName, sessionID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestR4FailedBatchMarksEverySessionFailed(t *testing.T) {
	store, _, batchName, _ := r4BatchStore(t, "a", "b")
	_, failed, err := applySummaryBatchResults(store, nil, nil, batchName, nil)
	if err != nil || failed != 2 {
		t.Fatalf("apply failed batch = failed:%d err:%v, want 2 nil", failed, err)
	}
	for _, id := range []string{"a", "b"} {
		session, err := store.GetSession(id)
		if err != nil || session.SummaryStatus != storage.SummaryFailed || r4BatchState(t, store, batchName, id) != storage.SummaryFailed {
			t.Fatalf("failed batch left %s as session=%+v err=%v state=%q", id, session, err, r4BatchState(t, store, batchName, id))
		}
	}
}

func TestR4PartialBatchMarksUnreturnedSessionFailed(t *testing.T) {
	store, _, batchName, _ := r4BatchStore(t, "a", "b")
	succeeded, failed, err := applySummaryBatchResults(store, nil, nil, batchName, []summary.BatchResult{{SessionID: "a", Title: "A", Summary: "A summary", PresetName: "default", SpeakerNames: "{}"}})
	if err != nil || succeeded != 1 || failed != 1 {
		t.Fatalf("apply partial batch = succeeded:%d failed:%d err:%v, want 1 1 nil", succeeded, failed, err)
	}
	if state := r4BatchState(t, store, batchName, "a"); state != storage.SummaryCompleted {
		t.Fatalf("returned session state = %q, want completed", state)
	}
	if session, err := store.GetSession("b"); err != nil || session.SummaryStatus != storage.SummaryFailed || r4BatchState(t, store, batchName, "b") != storage.SummaryFailed {
		t.Fatalf("unreturned session = %+v err=%v state=%q, want failed", session, err, r4BatchState(t, store, batchName, "b"))
	}
}

func TestR4BatchStatesSurviveRestartBeforePoll(t *testing.T) {
	store, path, batchName, _ := r4BatchStore(t, "a")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := storage.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, failed, err := applySummaryBatchResults(store, nil, nil, batchName, nil); err != nil || failed != 1 {
		t.Fatalf("apply after restart = failed:%d err:%v, want 1 nil", failed, err)
	}
	if state := r4BatchState(t, store, batchName, "a"); state != storage.SummaryFailed {
		t.Fatalf("state after restart poll = %q, want failed", state)
	}
}

func TestR4BatchSubmitReadRefusesDeleteBetweenReadAndSubmit(t *testing.T) {
	store, _, _, _ := r4BatchStore(t, "a")
	sessions, generations, _, err := readBatchSummarySessions(store, []string{"a"})
	if err != nil || len(sessions) != 1 || sessions[0].Transcript == "" {
		t.Fatalf("batch input = sessions:%+v generations:%v err:%v", sessions, generations, err)
	}
	segments, err := store.GetSegments("a")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteSegment("a", segments[0].ID, storage.DeletedSpan{AudioPath: "recording.mp3", Start: 0, End: 1, SHA256Before: "before", SHA256After: "after"}); err != nil {
		t.Fatal(err)
	}
	if err := store.StartSummary("a"); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateSummaryAtGeneration("a", "stale", sessions[0].Transcript, storage.SummaryCompleted, "default", "{}", generations["a"]); err == nil {
		t.Fatal("batch submit retained a transcript after its generation changed")
	}
}

func TestR4StaleBatchResultIsNotBroadcast(t *testing.T) {
	store, _, batchName, _ := r4BatchStore(t, "a")
	segments, err := store.GetSegments("a")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteSegment("a", segments[0].ID, storage.DeletedSpan{AudioPath: "recording.mp3", Start: 0, End: 1, SHA256Before: "before", SHA256After: "after"}); err != nil {
		t.Fatal(err)
	}
	if err := store.StartSummary("a"); err != nil {
		t.Fatal(err)
	}
	hub := &r4BatchHub{}
	published := 0
	_, failed, err := applySummaryBatchResults(store, hub, func(string) { published++ }, batchName, []summary.BatchResult{{SessionID: "a", Title: "stale", Summary: "private", PresetName: "default", SpeakerNames: "{}"}})
	if err != nil || failed != 1 {
		t.Fatalf("apply stale batch = failed:%d err:%v, want 1 nil", failed, err)
	}
	if len(hub.ready) != 0 || published != 0 {
		t.Fatalf("stale batch broadcast=%v published=%d, want neither", hub.ready, published)
	}
	if state := r4BatchState(t, store, batchName, "a"); state != "discarded" {
		t.Fatalf("stale batch state = %q, want discarded", state)
	}
}
