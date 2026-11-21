package storage

import (
	"path/filepath"
	"testing"
	"time"
)

// TestRefinedWordsKeepTheirModel fails if a published word does not keep the
// model that timed it, falling back to the session's when it names none, or if
// a "Delete text" records another model than its deleted words' own.
func TestRefinedWordsKeepTheirModel(t *testing.T) {
	store := newTestSQLiteStore(t)
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	if err := store.CreateSession("s", started); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("s", started.Add(time.Minute), "/rec/a.mp3,/rec/b.mp3", ""); err != nil {
		t.Fatal(err)
	}
	words := []RefinedWord{
		{RecordingPath: "/rec/a.mp3", RecordingSHA256: "a", Ordinal: 0, Speaker: 0, Text: "Older.", Start: 0, End: 0.5, Model: "nova-2"},
		{RecordingPath: "/rec/b.mp3", RecordingSHA256: "b", RecordingOrdinal: 1, Ordinal: 0, Speaker: 1, Text: "Newer.", Start: 0, End: 0.5},
	}
	revision, err := store.PublishRefinementAtGeneration("s", RefinedTranscript(words), "nova-3", words, BuildRefinedSegments(words), 0)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := store.RefinedWords("s", revision)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 2 || stored[0].Model != "nova-2" || stored[1].Model != "nova-3" {
		t.Fatalf("stored words = %+v, want a.mp3's word under nova-2 and b.mp3's under the session's nova-3", stored)
	}

	segments, err := store.GetSegments("s")
	if err != nil {
		t.Fatal(err)
	}
	for i := range segments {
		if segments[i].Text == "Older." {
			if err := store.DeleteRefinedSegmentText("s", segments[i].ID, revision); err != nil {
				t.Fatal(err)
			}
		}
	}
	spans, err := store.TextDeletionSpans("/rec/a.mp3")
	if err != nil || len(spans) != 1 || spans[0].Model != "nova-2" {
		t.Fatalf("a.mp3's deletion records = %+v (%v), want one under nova-2, its words' model, not the session's", spans, err)
	}
}

// TestRefinedWordsFromBeforeModelsTakeTheirSessionsModel fails if a word
// stored before refined words had a model is left with none on upgrade: a
// "Delete text" of it would then record no model, and its session could not
// be refined again.
func TestRefinedWordsFromBeforeModelsTakeTheirSessionsModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upgrade.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	if err := store.CreateSession("s", started); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("s", started.Add(time.Minute), "/rec/a.mp3", ""); err != nil {
		t.Fatal(err)
	}
	words := []RefinedWord{{RecordingPath: "/rec/a.mp3", RecordingSHA256: "a", Ordinal: 0, Text: "Hello.", Start: 0, End: 0.5}}
	revision, err := store.PublishRefinementAtGeneration("s", RefinedTranscript(words), "nova-2", words, BuildRefinedSegments(words), 0)
	if err != nil {
		t.Fatal(err)
	}
	// As a database written before the column existed holds it.
	if _, err := store.DB().Exec(`UPDATE refined_words SET model = ''`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	stored, err := store.RefinedWords("s", revision)
	if err != nil || len(stored) != 1 || stored[0].Model != "nova-2" {
		t.Fatalf("words after the upgrade = %+v (%v), want the session's nova-2", stored, err)
	}
}
