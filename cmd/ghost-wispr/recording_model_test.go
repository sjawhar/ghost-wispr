package main

// The #65 round 4 review's deleted-words path: a batch-model change before a
// merge leaves a merged session whose recordings were refined under two
// models. "Delete text" of a short segment, "Delete text and audio" of its
// touching neighbour, whose pad cuts the first deletion's record out, then
// another "Delete text" in that recording, then a refinement: the last
// deletion must be recorded, and refined, under the model its words came
// from, or the refinement brings its words back.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/session"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// byModel hears each recording, by file name, as its entry for the model
// asked says, as Deepgram's models time the same words differently.
type byModel struct {
	model   string
	mu      *sync.Mutex
	results map[string]map[string]transcribe.BatchResult // model -> recording -> result
}

func (b *byModel) Transcribe(_ context.Context, path string) (transcribe.BatchResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	result, ok := b.results[b.model][filepath.Base(path)]
	if !ok {
		return transcribe.BatchResult{}, os.ErrNotExist
	}
	return result, nil
}

func (b *byModel) WithModel(model string) transcribe.BatchTranscriber {
	return &byModel{model: model, mu: b.mu, results: b.results}
}

func (b *byModel) hear(model, recording string, words ...transcribe.TimestampedWord) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.results[model] == nil {
		b.results[model] = map[string]transcribe.BatchResult{}
	}
	b.results[model][recording] = transcribe.BatchResult{Model: model, Words: words}
}

func spoken(speaker int, text string, start, end float64) transcribe.TimestampedWord {
	return transcribe.TimestampedWord{Text: text, Start: start, End: end, Speaker: speaker}
}

func segmentSaying(t *testing.T, store *storage.SQLiteStore, sessionID, text string) (int64, int) {
	t.Helper()
	sess, err := store.GetSession(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	segments, err := store.GetSegments(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	for i := range segments {
		if strings.Contains(segments[i].Text, text) {
			return segments[i].ID, sess.RefinedRevision
		}
	}
	t.Fatalf("no segment of %s says %q: %+v", sessionID, text, segments)
	return 0, 0
}

func TestDeletedWordsStayGoneAfterAModelChangeMergeAndCut(t *testing.T) {
	dir := t.TempDir()
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	heard := &byModel{model: "nova-2", mu: &sync.Mutex{}, results: map[string]map[string]transcribe.BatchResult{}}
	manager := session.NewManager(store, nil, nil, nil, session.NewDetector(time.Hour), 0)
	manager.SetBatchTranscriber(heard)

	// Two sessions, one recording each, refined while the batch model is
	// nova-2.
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	recordings := map[string]string{}
	for i, id := range []string{"s1", "s2"} {
		path := filepath.Join(dir, id+".mp3")
		if err := os.WriteFile(path, []byte("audio of "+id), 0o600); err != nil {
			t.Fatal(err)
		}
		recordings[id] = path
		if err := store.CreateSession(id, started.Add(time.Duration(i)*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := store.EndSession(id, started.Add(time.Duration(i)*time.Hour+time.Minute), path); err != nil {
			t.Fatal(err)
		}
	}
	heard.hear("nova-2", "s1.mp3",
		spoken(0, "So", 0.2, 0.5), spoken(0, "we", 0.6, 0.8), spoken(0, "met.", 0.9, 1.2),
		spoken(1, "Okay.", 2.0, 2.3),
		spoken(0, "Orange", 2.4, 2.8), spoken(0, "tractor", 2.9, 3.2), spoken(0, "ninety.", 3.3, 3.6),
		spoken(1, "Purple", 5.0, 5.3), spoken(1, "wagon.", 5.4, 5.8),
		spoken(0, "Then", 7.0, 7.3), spoken(0, "left.", 7.4, 7.6))
	heard.hear("nova-2", "s2.mp3", spoken(0, "Other", 0.2, 0.5), spoken(0, "talk.", 0.6, 0.9))
	if err := manager.RefineSession(ctx, "s1", recordings["s1"], started); err != nil {
		t.Fatalf("refine s1: %v", err)
	}

	// 1. "Delete text" of the short segment "Okay." under nova-2.
	okay, revision := segmentSaying(t, store, "s1", "Okay.")
	if err := store.DeleteRefinedSegmentText("s1", okay, revision); err != nil {
		t.Fatalf("Delete text of Okay.: %v", err)
	}

	// 2. The batch model becomes nova-3, and the sessions merge. The merged
	// session refines s1's recording with nova-2, its deletion's model, and
	// s2's with nova-3.
	heard.mu.Lock()
	heard.model = "nova-3"
	heard.mu.Unlock()
	heard.hear("nova-3", "s2.mp3", spoken(0, "Other", 0.25, 0.55), spoken(0, "talk.", 0.65, 0.95))
	if err := store.MergeSessions("m", []string{"s1", "s2"}, started, started.Add(2*time.Hour)); err != nil {
		t.Fatalf("merge: %v", err)
	}
	merged, err := store.GetSession("m")
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.RefineSession(ctx, "m", merged.AudioPath, merged.StartedAt); err != nil {
		t.Fatalf("refine m: %v", err)
	}
	if merged, err = store.GetSession("m"); err != nil || merged.RefinementStatus != storage.RefinementCompleted {
		t.Fatalf("merged refinement = %q (%v), want completed", merged.RefinementStatus, err)
	}

	// 3. "Delete text and audio" of the touching neighbour: its 0.45 s pad
	// reaches back over "Okay.", so the cut drops that deletion's record.
	var checkModels []string
	runner := testDeleteRunner(t, &segmentDeleteRunner{
		store: store,
		ensureRecordings: func(context.Context, storage.Session) (deletionRecordingSet, error) {
			return deletionRecordingSet{Paths: []string{recordings["s1"], recordings["s2"]}}, nil
		},
		splice: func(_ context.Context, path string, start, end float64) (string, error) {
			if err := os.WriteFile(path, []byte("s1 without its tractor"), 0o600); err != nil {
				return "", err
			}
			return fileSHA256(path)
		},
		transcribeCut: func(_ context.Context, model, _ string) ([]transcribe.TimestampedWord, error) {
			checkModels = append(checkModels, model)
			return []transcribe.TimestampedWord{spoken(0, "So", 0.2, 0.5), spoken(1, "Purple", 2.9, 3.2)}, nil
		},
	})
	tractor, revision := segmentSaying(t, store, "m", "tractor")
	if err := runner.DeleteRefinedAudio(ctx, "m", tractor, revision); err != nil {
		t.Fatalf("Delete text and audio of the tractor segment: %v", err)
	}
	if len(checkModels) != 1 || checkModels[0] != "nova-2" {
		t.Fatalf("the check after the cut transcribed with %q, want nova-2, the model that timed s1.mp3's words", checkModels)
	}
	if spans, err := store.TextDeletionSpans(recordings["s1"]); err != nil || len(spans) != 0 {
		t.Fatalf("deletion records of s1.mp3 after the cut = %+v (%v), want the cut to have dropped them", spans, err)
	}

	// 4. "Delete text" of "Purple wagon.", now at 2.9 to 3.7 s.
	wagon, revision := segmentSaying(t, store, "m", "wagon")
	if err := store.DeleteRefinedSegmentText("m", wagon, revision); err != nil {
		t.Fatalf("Delete text of the wagon segment: %v", err)
	}

	// 5. A refinement. nova-2 times the deleted words inside the deletion's
	// span, as it did when the person deleted them; nova-3 times "wagon."
	// just past it.
	heard.hear("nova-2", "s1.mp3",
		spoken(0, "So", 0.2, 0.5), spoken(0, "we", 0.6, 0.8), spoken(0, "met.", 0.9, 1.2),
		spoken(1, "Purple", 2.9, 3.2), spoken(1, "wagon.", 3.3, 3.7),
		spoken(0, "Then", 4.9, 5.2), spoken(0, "left.", 5.3, 5.5))
	heard.hear("nova-3", "s1.mp3",
		spoken(0, "So", 0.2, 0.5), spoken(0, "we", 0.6, 0.8), spoken(0, "met.", 0.9, 1.2),
		spoken(1, "Purple", 2.95, 3.3), spoken(1, "wagon.", 3.75, 4.1),
		spoken(0, "Then", 4.9, 5.2), spoken(0, "left.", 5.3, 5.5))
	merged, err = store.GetSession("m")
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.RefineSession(ctx, "m", merged.AudioPath, merged.StartedAt); err != nil {
		t.Fatalf("refine m again: %v", err)
	}
	if merged, err = store.GetSession("m"); err != nil {
		t.Fatal(err)
	}
	for _, word := range []string{"Purple", "wagon", "tractor", "Okay"} {
		if strings.Contains(merged.CanonicalTranscript, word) || strings.Contains(merged.RefinedTranscript, word) {
			t.Fatalf("after the merge, the cut and a refinement, the transcript has the deleted word %q: refinement %q, canonical %q",
				word, merged.RefinementStatus, merged.CanonicalTranscript)
		}
	}
	if merged.RefinementStatus != storage.RefinementCompleted || !strings.Contains(merged.CanonicalTranscript, "Then left.") {
		t.Fatalf("refinement = %q, canonical %q; want completed with the kept words", merged.RefinementStatus, merged.CanonicalTranscript)
	}
}
