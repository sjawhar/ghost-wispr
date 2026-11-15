package storage

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func publishHolderScrubWords(t *testing.T, store *SQLiteStore, id string, words []RefinedWord) int {
	t.Helper()
	if _, err := store.DB().Exec(`UPDATE sessions SET refinement_generation = deletion_generation, status = 'ended' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	generation, err := store.DeletionGeneration(id)
	if err != nil {
		t.Fatal(err)
	}
	revision, err := store.PublishRefinementAtGeneration(id, RefinedTranscript(words), "test", words, BuildRefinedSegments(words), generation)
	if err != nil {
		t.Fatal(err)
	}
	return revision
}

func TestRefinedDeleteScrubsHolderWhoseOnlyRecordingWasDeleted(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "holder.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	const firstID, secondID = "20261106100000", "20261106110000"
	firstRecording := "/audio/" + firstID + ".mp3"
	secondRecording := "/audio/" + secondID + ".mp3"
	for id, at := range map[string]time.Time{firstID: started, secondID: started.Add(time.Hour)} {
		if err := store.CreateSession(id, at); err != nil {
			t.Fatal(err)
		}
		if err := store.EndSession(id, at.Add(time.Minute), "/audio/"+id+".mp3"); err != nil {
			t.Fatal(err)
		}
	}
	firstWords := []RefinedWord{{RecordingPath: firstRecording, RecordingSHA256: "first", Ordinal: 0, Text: "Xraysecret only.", Start: 0, End: 1}}
	secondWords := []RefinedWord{{RecordingPath: secondRecording, RecordingSHA256: "second", Ordinal: 0, Text: "Charlie stays.", Start: 0, End: 1}}
	publishHolderScrubWords(t, store, firstID, firstWords)
	publishHolderScrubWords(t, store, secondID, secondWords)
	generation, err := store.DeletionGeneration(firstID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateSummaryAtGeneration(firstID, "About Xraysecret", "The summary of Xraysecret.", SummaryCompleted, "default", "{}", "", generation); err != nil {
		t.Fatal(err)
	}
	if err := store.MergeSessions("merged", []string{firstID, secondID}, started, started.Add(61*time.Minute)); err != nil {
		t.Fatal(err)
	}
	mergedWords := append([]RefinedWord{}, firstWords...)
	second := secondWords[0]
	second.RecordingOrdinal = 1
	mergedWords = append(mergedWords, second)
	revision := publishHolderScrubWords(t, store, "merged", mergedWords)
	segments, err := store.GetSegments("merged")
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 2 {
		t.Fatalf("merged segments = %d, want 2", len(segments))
	}
	if err := store.DeleteRefinedSegmentText("merged", segments[0].ID, revision); err != nil {
		t.Fatal(err)
	}
	first, err := store.GetSession(firstID)
	if err != nil {
		t.Fatal(err)
	}
	for field, text := range map[string]string{
		"canonical transcript": first.CanonicalTranscript,
		"title":                first.Title,
		"summary":              first.Summary,
		"refined transcript":   first.RefinedTranscript,
	} {
		if strings.Contains(text, "Xraysecret") {
			t.Errorf("holder's %s still contains the deleted words: %q", field, text)
		}
	}
	var matches int
	if err := store.DB().QueryRow(`SELECT count(*) FROM sessions_fts WHERE sessions_fts MATCH 'Xraysecret'`).Scan(&matches); err != nil {
		t.Fatal(err)
	}
	if matches != 0 {
		t.Errorf("search still finds Xraysecret in %d sessions", matches)
	}
}
