package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// A retried refinement makes the session's summary again. When that summary
// fails, the session keeps its title and its last good summary, and the
// failure is recorded as its summary's status and the error; the summary
// announced last is the one it keeps.
func TestAFailedSummaryAfterARefinementKeepsTheLastGoodSummary(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	const id = "20261106110000"
	started := time.Date(2026, 11, 6, 11, 0, 0, 0, time.UTC)
	recording := filepath.Join(t.TempDir(), id+".mp3")
	if err := os.WriteFile(recording, []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(id, started); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := store.AppendSegment(id, transcribe.Segment{Text: "We agreed to move the launch to Friday.", StartTime: 0, EndTime: 3, Timestamp: started}); err != nil {
		t.Fatalf("append segment: %v", err)
	}
	if err := store.EndSession(id, started.Add(time.Minute), recording); err != nil {
		t.Fatalf("end session: %v", err)
	}
	const speakers = `{"0":{"name":"Sam","confidence":"high"}}`
	if err := store.UpdateSummary(id, "Launch planning", "The last good summary.", storage.SummaryCompleted, "default", speakers); err != nil {
		t.Fatalf("summarize: %v", err)
	}

	hub := &hubMock{}
	cause := errors.New("summarize: model unavailable (503)")
	manager := NewManager(store, nil, failingSummarizerMock{err: cause}, hub, NewDetector(time.Hour), 0)
	manager.SetBatchTranscriber(batchTranscriberMock{transcript: "We agreed to move the launch to Friday, refined."})
	if err := manager.RefineSession(context.Background(), id, recording, started); err != nil {
		t.Fatalf("RefineSession: %v", err)
	}

	sess, err := store.GetSession(id)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if sess.Title != "Launch planning" || sess.Summary != "The last good summary." ||
		sess.SummaryPreset != "default" || sess.SpeakerNames != speakers {
		t.Errorf("after the failed summary: title %q, summary %q, preset %q, speakers %s; want the last good ones kept",
			sess.Title, sess.Summary, sess.SummaryPreset, sess.SpeakerNames)
	}
	if sess.SummaryStatus != storage.SummaryFailed || !strings.Contains(sess.SummaryError, cause.Error()) {
		t.Errorf("failure recorded as summary_status %q, summary_error %q; want %q and the error", sess.SummaryStatus, sess.SummaryError, storage.SummaryFailed)
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if hub.latestStatus != storage.SummaryFailed || hub.latestTitle != "Launch planning" || hub.latestSummary != "The last good summary." || hub.latestPreset != "default" {
		t.Errorf("last announcement: status %q, title %q, summary %q, preset %q; want failed with the kept title, summary and preset",
			hub.latestStatus, hub.latestTitle, hub.latestSummary, hub.latestPreset)
	}
}
