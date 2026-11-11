package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// failingSummarizer fails as the real summarizer does when its model call
// fails: with an error, and a title made from the transcript.
type failingSummarizer struct{ err error }

func (f failingSummarizer) Summarize(context.Context, string, string) (string, string, string, string, error) {
	return "We agreed to move the launch", "", "", "{}", f.err
}

func (f failingSummarizer) SummarizeWithPreset(context.Context, string, string, string) (string, string, string, error) {
	return "We agreed to move the launch", "", "{}", f.err
}

// summaryEvent is one announcement of a session's summary.
type summaryEvent struct{ sessionID, title, summary, status, preset string }

// recordingBroadcaster keeps every summary announcement.
type recordingBroadcaster struct {
	mu     sync.Mutex
	events []summaryEvent
}

func (r *recordingBroadcaster) BroadcastSummaryReady(sessionID, title, summary, status, preset string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, summaryEvent{sessionID, title, summary, status, preset})
}

// A summary made again that fails takes nothing away: the session keeps its
// title and its last good summary (the ones Drive holds), and the failure is
// recorded as its summary's status and the error. Its documents do not
// change, so it stays synced, and the last announcement of its summary says
// what it keeps.
func TestAFailedResummarizeKeepsTheLastGoodSummaryAndTitle(t *testing.T) {
	for _, preset := range []string{"", "detailed"} {
		t.Run("preset "+preset, func(t *testing.T) {
			store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			t.Cleanup(func() { _ = store.Close() })
			const id = "20261106090000"
			started := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
			if err := store.CreateSession(id, started); err != nil {
				t.Fatalf("create session: %v", err)
			}
			if err := store.AppendSegment(id, transcribe.Segment{Text: "We agreed to move the launch to Friday.", StartTime: 0, EndTime: 3, Timestamp: started}); err != nil {
				t.Fatalf("append segment: %v", err)
			}
			if err := store.EndSession(id, started.Add(time.Minute), ""); err != nil {
				t.Fatalf("end session: %v", err)
			}
			if _, err := store.Canonicalize(id); err != nil {
				t.Fatalf("canonicalize: %v", err)
			}
			const speakers = `{"0":{"name":"Sam","confidence":"high"}}`
			if err := store.UpdateSummary(id, "Launch planning", "The last good summary.", storage.SummaryCompleted, "default", speakers); err != nil {
				t.Fatalf("summarize: %v", err)
			}
			if err := store.UpdateTitle(id, "Launch planning, renamed"); err != nil {
				t.Fatalf("rename: %v", err)
			}
			syncTo(t, store, id, storage.SyncStateSynced, "folder-1", 0, "")

			hub := &recordingBroadcaster{}
			published := false
			cause := errors.New("summarize: model unavailable (503)")
			err = resummarize(context.Background(), store, failingSummarizer{err: cause}, hub, func(string) { published = true }, id, preset)
			if !errors.Is(err, cause) {
				t.Errorf("resummarize: %v, want %v", err, cause)
			}

			sess, err := store.GetSession(id)
			if err != nil {
				t.Fatalf("get session: %v", err)
			}
			if sess.Title != "Launch planning, renamed" || sess.Summary != "The last good summary." ||
				sess.SummaryPreset != "default" || sess.SpeakerNames != speakers {
				t.Errorf("after the failed summary: title %q, summary %q, preset %q, speakers %s; want the last good ones kept",
					sess.Title, sess.Summary, sess.SummaryPreset, sess.SpeakerNames)
			}
			if sess.SummaryStatus != storage.SummaryFailed || !strings.Contains(sess.SummaryError, cause.Error()) {
				t.Errorf("failure recorded as summary_status %q, summary_error %q; want %q and the error", sess.SummaryStatus, sess.SummaryError, storage.SummaryFailed)
			}
			if sess.SyncState != storage.SyncStateSynced {
				t.Errorf("sync state %q, want %q: its documents did not change", sess.SyncState, storage.SyncStateSynced)
			}
			if published {
				t.Error("a failed summary was published")
			}
			hub.mu.Lock()
			defer hub.mu.Unlock()
			last := hub.events[len(hub.events)-1]
			if want := (summaryEvent{id, "Launch planning, renamed", "The last good summary.", storage.SummaryFailed, "default"}); last != want {
				t.Errorf("last announcement %+v, want %+v", last, want)
			}
		})
	}
}
