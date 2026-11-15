package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// recordingModelTranscriber hears each recording as its entry for the model
// asked says, by recording file name, and fails for a model it has no
// entries for, as Deepgram answers a retired model with HTTP 400.
type recordingModelTranscriber struct {
	model   string
	results map[string]map[string]transcribe.BatchResult // model -> recording -> result
	mu      *sync.Mutex
	asked   *[]string // "model recording"
}

func (m *recordingModelTranscriber) Transcribe(_ context.Context, path string) (transcribe.BatchResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	*m.asked = append(*m.asked, m.model+" "+filepath.Base(path))
	byRecording, ok := m.results[m.model]
	if !ok {
		return transcribe.BatchResult{}, errors.New("deepgram batch transcription: HTTP 400 Bad Request: no such model")
	}
	return byRecording[filepath.Base(path)], nil
}

func (m *recordingModelTranscriber) WithModel(model string) transcribe.BatchTranscriber {
	return &recordingModelTranscriber{model: model, results: m.results, mu: m.mu, asked: m.asked}
}

// twoRecordingSession makes an ended session id over recordings a.mp3 and
// b.mp3, with a "Delete text" span from 1.0 to 1.95 s in each, recorded under
// modelA and modelB, and returns the store, the session's audio path and its
// start.
func twoRecordingSession(t *testing.T, id, modelA, modelB string) (*storage.SQLiteStore, string, time.Time) {
	t.Helper()
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	dir := t.TempDir()
	started := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	for name, model := range map[string]string{"a.mp3": modelA, "b.mp3": modelB} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("audio of "+name), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := store.DB().Exec(`INSERT INTO deleted_text_spans(session_id, recording, start, end, sha256, deleted_at, model) VALUES(?, ?, 1.0, 1.95, ?, '2026-11-12T00:00:00Z', ?)`,
			id, name, fileHash(t, path), model); err != nil {
			t.Fatal(err)
		}
	}
	audioPath := filepath.Join(dir, "a.mp3") + "," + filepath.Join(dir, "b.mp3")
	if err := store.CreateSession(id, started); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession(id, started.Add(time.Minute), audioPath); err != nil {
		t.Fatal(err)
	}
	return store, audioPath, started
}

// heardIn is what a model hears in a recording of twoRecordingSession: its
// kept words, and the deleted ones inside the span.
func heardIn(model, name string) transcribe.BatchResult {
	return refinementResult(model, timedWords(0, "Keep "+name+"."), timedWords(1.0, "Gone "+name+"."))
}

// TestRefinementUsesEachRecordingsDeletionModel fails if a session whose
// recordings had text deleted under two models, as after a merge across a
// change of the batch model, is refused for good, which blocked every later
// deletion in it: each recording is refined with its own spans' model.
func TestRefinementUsesEachRecordingsDeletionModel(t *testing.T) {
	store, audioPath, started := twoRecordingSession(t, "20261112100000", "nova-2", "nova-3")
	transcriber := &recordingModelTranscriber{model: "nova-3", mu: &sync.Mutex{}, asked: &[]string{}, results: map[string]map[string]transcribe.BatchResult{
		"nova-2": {"a.mp3": heardIn("nova-2", "a"), "b.mp3": heardIn("nova-2", "b")},
		"nova-3": {"a.mp3": heardIn("nova-3", "a"), "b.mp3": heardIn("nova-3", "b")},
	}}
	manager := NewManager(store, nil, nil, nil, NewDetector(time.Hour), 0)
	manager.SetBatchTranscriber(transcriber)
	if err := manager.RefineSession(context.Background(), "20261112100000", audioPath, started); err != nil {
		t.Fatalf("RefineSession: %v", err)
	}
	sess, err := store.GetSession("20261112100000")
	if err != nil {
		t.Fatal(err)
	}
	if sess.RefinementStatus != storage.RefinementCompleted || sess.RefinedTranscript != "Keep a.\n\nKeep b." {
		t.Fatalf("refinement = %q %q, want completed with \"Keep a.\" and \"Keep b.\"", sess.RefinementStatus, sess.RefinedTranscript)
	}
	if got := strings.Join(*transcriber.asked, ","); got != "nova-2 a.mp3,nova-3 b.mp3" {
		t.Fatalf("transcriptions = %q, want each recording with its own deletion's model", got)
	}

	// A later "Delete text" in each recording records the model that
	// recording's words came from.
	for {
		sess, err := store.GetSession("20261112100000")
		if err != nil {
			t.Fatal(err)
		}
		segments, err := store.GetSegments("20261112100000")
		if err != nil {
			t.Fatal(err)
		}
		if len(segments) == 0 {
			break
		}
		if err := store.DeleteRefinedSegmentText("20261112100000", segments[0].ID, sess.RefinedRevision); err != nil {
			t.Fatalf("Delete text of %q: %v", segments[0].Text, err)
		}
	}
	for name, want := range map[string]string{"a.mp3": "nova-2", "b.mp3": "nova-3"} {
		spans, err := store.TextDeletionSpans(filepath.Join(filepath.Dir(strings.Split(audioPath, ",")[0]), name))
		if err != nil {
			t.Fatal(err)
		}
		for _, span := range spans {
			if span.Model != want {
				t.Errorf("%s: a later \"Delete text\" recorded model %q, want %q, the model its words came from", name, span.Model, want)
			}
		}
	}
}

// TestRefinementWarnsWhenTheDeletionModelFails fails if a refinement whose
// deletion model Deepgram no longer serves fails without a status warning
// naming the session and the model.
func TestRefinementWarnsWhenTheDeletionModelFails(t *testing.T) {
	store, recording, started := deletedTextSessionWithModel(t, "20261112100000", "audio", "nova-2")
	transcriber := &recordingModelTranscriber{model: "nova-3", mu: &sync.Mutex{}, asked: &[]string{}, results: map[string]map[string]transcribe.BatchResult{
		"nova-3": {filepath.Base(recording): crossModelWords("nova-3")},
	}}
	manager := NewManager(store, nil, nil, nil, NewDetector(time.Hour), 0)
	manager.SetBatchTranscriber(transcriber)
	var warnings []string
	manager.SetRefinementWarningHook(func(sessionID, warning string) {
		warnings = append(warnings, sessionID+": "+warning)
	})
	_ = manager.RefineSession(context.Background(), "20261112100000", recording, started)
	sess, err := store.GetSession("20261112100000")
	if err != nil {
		t.Fatal(err)
	}
	if sess.RefinementStatus == storage.RefinementCompleted || strings.Contains(sess.RefinedTranscript, "tractor") {
		t.Fatalf("refinement = %q %q, want refused, publishing nothing", sess.RefinementStatus, sess.RefinedTranscript)
	}
	if len(warnings) != 1 || !strings.HasPrefix(warnings[0], "20261112100000: ") || !strings.Contains(warnings[0], "nova-2") || !strings.Contains(warnings[0], "400") {
		t.Fatalf("refusal warnings = %q, want one naming the session, the model and Deepgram's answer", warnings)
	}
}
