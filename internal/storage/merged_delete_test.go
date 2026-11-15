package storage

import (
	"path/filepath"
	"strings"
	"testing"
)

// mergedSourceSecret is one search token, so it is found in the full-text
// index as well as in rows.
const mergedSourceSecret = "zebraquartzsecret"

// TestDeleteRefinedSegmentAudioScrubsMergedSources fails if deleting a
// refined occurrence of a merged session leaves its words in a source row,
// the search index, stored events, embeddings or database files.
func TestDeleteRefinedSegmentAudioScrubsMergedSources(t *testing.T) {
	path := filepath.Join(t.TempDir(), "merged.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	target := mergedSessionWithSecret(t, store)
	span := DeletedSpan{AudioPath: "recording-source-a.mp3", Start: 2, End: 4, SHA256Before: "a-sha", SHA256After: "after"}
	if err := store.DeleteRefinedSegmentAudio("merged", target.id, target.revision, &span); err != nil {
		t.Fatalf("DeleteRefinedSegmentAudio: %v", err)
	}

	source := storeSession(t, store, "source-a")
	for field, value := range map[string]string{"title": source.Title, "summary": source.Summary, "canonical transcript": source.CanonicalTranscript, "refined transcript": source.RefinedTranscript} {
		if strings.Contains(value, mergedSourceSecret) {
			t.Errorf("the merged source's %s still holds the deleted words: %q", field, value)
		}
	}
	if !strings.Contains(source.CanonicalTranscript, "alpha stays") {
		t.Errorf("the merged source's canonical transcript lost the words it keeps: %q", source.CanonicalTranscript)
	}
	var indexed, embeddings int
	if err := store.DB().QueryRow(`SELECT count(*) FROM sessions_fts WHERE sessions_fts MATCH ?`, mergedSourceSecret).Scan(&indexed); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRow(`SELECT count(*) FROM embeddings WHERE session_id = 'source-a'`).Scan(&embeddings); err != nil {
		t.Fatal(err)
	}
	if indexed != 0 || embeddings != 0 {
		t.Errorf("after the deletion: %d search index matches, %d source embeddings; want none", indexed, embeddings)
	}
	assertDatabaseFilesLack(t, path, mergedSourceSecret, "alpha stays")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}
