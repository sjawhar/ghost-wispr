package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// deletedWords is the text of the segment the tests delete.
const deletedWords = "private deletion token"

// racingSummarizer summarizes the transcript it is given, title included, and
// runs during (once) while the model call is out, before it answers.
type racingSummarizer struct {
	during func()
	err    error
}

func (r *racingSummarizer) answer(transcript string) (title, summary string) {
	if r.during != nil {
		during := r.during
		r.during = nil
		during()
	}
	text := strings.Join(strings.Fields(transcript), " ")
	return "About " + text, "They said: " + text
}

func (r *racingSummarizer) Summarize(_ context.Context, _, transcript string) (string, string, string, string, error) {
	title, summary := r.answer(transcript)
	return title, summary, "default", "{}", r.err
}

func (r *racingSummarizer) SummarizeWithPreset(_ context.Context, _, transcript, _ string) (string, string, string, error) {
	title, summary := r.answer(transcript)
	return title, summary, "{}", r.err
}

func (r *racingSummarizer) SummarizeReported(_ context.Context, _, transcript string) (string, string, string, string, string, error) {
	title, summary := r.answer(transcript)
	return title, summary, "default", "{}", "model-old-run", r.err
}

func (r *racingSummarizer) SummarizeWithPresetReported(_ context.Context, _, transcript, _ string) (string, string, string, string, error) {
	title, summary := r.answer(transcript)
	return title, summary, "{}", "model-old-run", r.err
}

// deleteAndStartNewerSummary deletes segment segmentID from session id, as
// the API does, and starts the summary the deletion then makes of the new
// text (StartSummary): the newer run that an older one must leave alone.
func deleteAndStartNewerSummary(t *testing.T, store *storage.SQLiteStore, id string, segmentID int64) {
	t.Helper()
	if err := store.DeleteSegment(id, segmentID, storage.DeletedSpan{AudioPath: "recording.mp3", Start: 1, End: 2, SHA256Before: "before", SHA256After: "after"}); err != nil {
		t.Fatalf("delete segment: %v", err)
	}
	if err := store.StartSummary(id); err != nil {
		t.Fatalf("start the newer summary: %v", err)
	}
}

// writeHookStore runs beforeWrite (once) when a summary write reaches it,
// before the write: what happens after the writer read the session.
type writeHookStore struct {
	*storage.SQLiteStore
	beforeWrite func()
}

func (w *writeHookStore) hook() {
	if w.beforeWrite != nil {
		beforeWrite := w.beforeWrite
		w.beforeWrite = nil
		beforeWrite()
	}
}

func (w *writeHookStore) UpdateSummaryAtGeneration(sessionID, title, summary, status, preset, speakerNames, model string, generation int) error {
	w.hook()
	return w.SQLiteStore.UpdateSummaryAtGeneration(sessionID, title, summary, status, preset, speakerNames, model, generation)
}

// A summary made from the text a session had before a segment deletion, which
// is stored after the deletion's own summary started, would bring the deleted
// words back. Each summary writer stores its summary only while the session
// still has the transcript generation it read with the text, so a stale one
// is refused and not published.
func TestASummaryOfTheTextBeforeADeletionIsNotStored(t *testing.T) {
	const id = "session"
	for _, writer := range []struct {
		name string
		run  func(t *testing.T, store *storage.SQLiteStore, segmentID int64, publish func(string))
	}{
		{"resummarize", func(t *testing.T, store *storage.SQLiteStore, segmentID int64, publish func(string)) {
			summarizer := &racingSummarizer{during: func() { deleteAndStartNewerSummary(t, store, id, segmentID) }}
			_ = resummarize(context.Background(), store, summarizer, &recordingBroadcaster{}, publish, id, "")
		}},
		{"resummarize with a preset", func(t *testing.T, store *storage.SQLiteStore, segmentID int64, publish func(string)) {
			summarizer := &racingSummarizer{during: func() { deleteAndStartNewerSummary(t, store, id, segmentID) }}
			_ = resummarize(context.Background(), store, summarizer, &recordingBroadcaster{}, publish, id, "detailed")
		}},
		{"merged session", func(t *testing.T, store *storage.SQLiteStore, segmentID int64, publish func(string)) {
			summarizer := &racingSummarizer{during: func() { deleteAndStartNewerSummary(t, store, id, segmentID) }}
			summarizeMergedSession(context.Background(), store, summarizer, &recordingBroadcaster{}, publish, id)
		}},
		{"startup", func(t *testing.T, store *storage.SQLiteStore, segmentID int64, publish func(string)) {
			summarizer := &racingSummarizer{during: func() { deleteAndStartNewerSummary(t, store, id, segmentID) }}
			summarizeBackfillSession(context.Background(), store, summarizer, publish, id)
		}},
		{"repair", func(t *testing.T, store *storage.SQLiteStore, segmentID int64, publish func(string)) {
			raw := `{"title":"About ` + deletedWords + `","summary":"They said: ` + deletedWords + `"}`
			if err := store.UpdateSummaryAtGeneration(id, "", raw, storage.SummaryCompleted, "default", "{}", "model-old-run", 0); err != nil {
				t.Fatalf("store a structured summary: %v", err)
			}
			hooked := &writeHookStore{SQLiteStore: store, beforeWrite: func() { deleteAndStartNewerSummary(t, store, id, segmentID) }}
			if _, err := repairStructuredSummaries(context.Background(), hooked, publish); err != nil {
				t.Logf("repair: %v", err)
			}
		}},
	} {
		t.Run(writer.name, func(t *testing.T) {
			store, segmentID, _ := newDeletionTestStore(t, "recording.mp3")
			published := false
			writer.run(t, store, segmentID, func(string) { published = true })

			sess, err := store.GetSession(id)
			if err != nil {
				t.Fatalf("get session: %v", err)
			}
			if strings.Contains(sess.Title, deletedWords) || strings.Contains(sess.Summary, deletedWords) {
				t.Errorf("after the deletion the session has title %q and summary %q: the deleted words came back", sess.Title, sess.Summary)
			}
			if published {
				t.Error("the stale summary was published")
			}
		})
	}
}

// A summary run that read the text from before a segment deletion, and ends
// after the deletion's own summary started, records nothing: neither a
// refused summary nor a failed model call marks the newer run failed, which
// would let a Drive replacement go ahead before the newer summary is stored.
func TestAStaleSummaryRunLeavesTheNewerRunRunning(t *testing.T) {
	const id = "session"
	modelDown := errors.New("summarize: model unavailable (503)")
	for _, writer := range []struct {
		name string
		run  func(store *storage.SQLiteStore, summarizer *racingSummarizer, hub *recordingBroadcaster)
	}{
		{"resummarize", func(store *storage.SQLiteStore, summarizer *racingSummarizer, hub *recordingBroadcaster) {
			_ = resummarize(context.Background(), store, summarizer, hub, func(string) {}, id, "")
		}},
		{"merged session", func(store *storage.SQLiteStore, summarizer *racingSummarizer, hub *recordingBroadcaster) {
			summarizeMergedSession(context.Background(), store, summarizer, hub, func(string) {}, id)
		}},
		{"startup", func(store *storage.SQLiteStore, summarizer *racingSummarizer, _ *recordingBroadcaster) {
			summarizeBackfillSession(context.Background(), store, summarizer, func(string) {}, id)
		}},
	} {
		for _, outcome := range []struct {
			name string
			err  error
		}{
			{"summary refused", nil},
			{"model call failed", modelDown},
		} {
			t.Run(writer.name+", "+outcome.name, func(t *testing.T) {
				store, segmentID, _ := newDeletionTestStore(t, "recording.mp3")
				summarizer := &racingSummarizer{during: func() { deleteAndStartNewerSummary(t, store, id, segmentID) }, err: outcome.err}
				hub := &recordingBroadcaster{}
				writer.run(store, summarizer, hub)

				sess, err := store.GetSession(id)
				if err != nil {
					t.Fatalf("get session: %v", err)
				}
				if sess.SummaryStatus != storage.SummaryRunning || sess.SummaryError != "" {
					t.Errorf("summary status %q, error %q; want the newer run's: %q, no error", sess.SummaryStatus, sess.SummaryError, storage.SummaryRunning)
				}
				hub.mu.Lock()
				defer hub.mu.Unlock()
				for _, event := range hub.events {
					if event.status == storage.SummaryFailed {
						t.Errorf("announced %+v for the newer run", event)
					}
				}
			})
		}
	}
}

// A batch summary completes, empty, the summary of a session whose transcript
// it read empty. Once a segment deletion has changed that session since, and
// its own summary run started, the empty completion is refused: it would
// mark the newer run completed before it stored anything.
func TestAnEmptyBatchSummaryLeavesARunStartedSinceRunning(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "empty.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	const id = "session"
	started := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	if err := store.CreateSession(id, started); err != nil {
		t.Fatalf("create session: %v", err)
	}
	for i := range 2 {
		if err := store.AppendSegment(id, transcribe.Segment{Text: " ", StartTime: float64(i), EndTime: float64(i + 1), Timestamp: started.Add(time.Duration(i) * time.Second)}); err != nil {
			t.Fatalf("append segment: %v", err)
		}
	}
	if err := store.EndSession(id, started.Add(2*time.Second), "recording.mp3"); err != nil {
		t.Fatalf("end session: %v", err)
	}
	segments, err := store.GetSegments(id)
	if err != nil {
		t.Fatalf("get segments: %v", err)
	}

	sessions, _, empty, err := readBatchSummarySessions(store, []string{id})
	if err != nil || len(sessions) != 0 || len(empty) != 1 {
		t.Fatalf("batch input: sessions %+v, empty %v, %v; want one empty transcript", sessions, empty, err)
	}
	deleteAndStartNewerSummary(t, store, id, segments[1].ID)
	completeEmptySummaries(store, empty)

	sess, err := store.GetSession(id)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if sess.SummaryStatus != storage.SummaryRunning {
		t.Errorf("summary status %q; want the newer run's: %q", sess.SummaryStatus, storage.SummaryRunning)
	}
}

// TestEmptySummaryWritesStartAtTheDeletionGeneration fails if an empty
// summary write after a deletion is refused because its writer did not first
// bind the empty run to the deletion's generation.
func TestEmptySummaryWritesStartAtTheDeletionGeneration(t *testing.T) {
	const id = "empty-after-delete"
	newStore := func(t *testing.T) *storage.SQLiteStore {
		t.Helper()
		store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "empty-after-delete.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		started := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
		if err := store.CreateSession(id, started); err != nil {
			t.Fatal(err)
		}
		if err := store.AppendSegment(id, transcribe.Segment{Text: deletedWords, StartTime: 0, EndTime: 1, Timestamp: started}); err != nil {
			t.Fatal(err)
		}
		if err := store.EndSession(id, started.Add(time.Second), "recording.mp3"); err != nil {
			t.Fatal(err)
		}
		segments, err := store.GetSegments(id)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.DeleteSegment(id, segments[0].ID, storage.DeletedSpan{AudioPath: "recording.mp3", Start: 0, End: 1, SHA256Before: "before", SHA256After: "after"}); err != nil {
			t.Fatal(err)
		}
		return store
	}
	for _, writer := range []struct {
		name string
		run  func(*storage.SQLiteStore)
	}{
		{"merged session", func(store *storage.SQLiteStore) {
			summarizeMergedSession(context.Background(), store, &racingSummarizer{}, &recordingBroadcaster{}, func(string) {}, id)
		}},
		{"startup", func(store *storage.SQLiteStore) {
			summarizeBackfillSession(context.Background(), store, &racingSummarizer{}, func(string) {}, id)
		}},
		{"batch", func(store *storage.SQLiteStore) {
			sessions, _, empty, err := readBatchSummarySessions(store, []string{id})
			if err != nil || len(sessions) != 0 || len(empty) != 1 {
				t.Fatalf("batch input: sessions %+v, empty %v, %v; want one empty transcript", sessions, empty, err)
			}
			completeEmptySummaries(store, empty)
		}},
	} {
		t.Run(writer.name, func(t *testing.T) {
			store := newStore(t)
			writer.run(store)
			sess, err := store.GetSession(id)
			if err != nil {
				t.Fatal(err)
			}
			if sess.SummaryStatus != storage.SummaryCompleted {
				t.Fatalf("summary status = %q, want %q after the empty completion", sess.SummaryStatus, storage.SummaryCompleted)
			}
		})
	}
}

// TestAStaleEmptyCompletionLeavesANewerCompletedSummary fails if an empty
// completion that read an earlier generation reopens a summary the deletion
// already completed at its newer generation.
func TestAStaleEmptyCompletionLeavesANewerCompletedSummary(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "stale-empty.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	const id = "stale-empty"
	started := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	if err := store.CreateSession(id, started); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		if err := store.AppendSegment(id, transcribe.Segment{Text: " ", StartTime: float64(i), EndTime: float64(i + 1), Timestamp: started.Add(time.Duration(i) * time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.EndSession(id, started.Add(2*time.Second), "recording.mp3"); err != nil {
		t.Fatal(err)
	}
	_, _, empty, err := readBatchSummarySessions(store, []string{id})
	if err != nil || len(empty) != 1 {
		t.Fatalf("read empty batch summary = %v, %v; want one session", empty, err)
	}
	segments, err := store.GetSegments(id)
	if err != nil {
		t.Fatal(err)
	}
	deleteAndStartNewerSummary(t, store, id, segments[1].ID)
	generation, err := store.DeletionGeneration(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateSummaryAtGeneration(id, "", "", storage.SummaryCompleted, "", "{}", "", generation); err != nil {
		t.Fatalf("complete newer empty summary: %v", err)
	}
	completeEmptySummaries(store, empty)
	session, err := store.GetSession(id)
	if err != nil {
		t.Fatal(err)
	}
	if session.SummaryStatus != storage.SummaryCompleted {
		t.Fatalf("summary status = %q, want the newer completed summary left alone", session.SummaryStatus)
	}
}
