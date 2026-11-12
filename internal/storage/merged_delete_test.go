package storage

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// mergedSourceSecret is one search token, so it is found in the full-text
// index as well as in rows.
const mergedSourceSecret = "zebraquartzsecret"

// TestDeleteSegmentScrubsMergedSources fails if deleting a segment of a
// merged session leaves its words in a source session's row (title, summary,
// canonical or refined transcript), in the search index, in the source's
// stored events or embeddings, or anywhere in the database files, as the
// A11 check reads them. The source's other words stay.
func TestDeleteSegmentScrubsMergedSources(t *testing.T) {
	path := filepath.Join(t.TempDir(), "merged.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	startA := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	startB := startA.Add(time.Hour)
	sessions := map[string][]string{
		"source-a": {"alpha stays", "the " + mergedSourceSecret + " words"},
		"source-b": {"charlie stays"},
	}
	for id, started := range map[string]time.Time{"source-a": startA, "source-b": startB} {
		if err := store.CreateSession(id, started); err != nil {
			t.Fatal(err)
		}
		for i, text := range sessions[id] {
			if err := store.AppendSegment(id, transcribe.Segment{Text: text, StartTime: float64(2 * i), EndTime: float64(2*i + 2), Timestamp: started.Add(time.Duration(2*i) * time.Second)}); err != nil {
				t.Fatal(err)
			}
		}
		if err := store.EndSession(id, started.Add(time.Minute), "recording-"+id+".mp3"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.Canonicalize(id); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.UpdateSummaryAtGeneration("source-a", "About "+mergedSourceSecret, "The summary of "+mergedSourceSecret+".", SummaryCompleted, "default", "{}", "", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET refined_transcript = ?, refinement_status = ? WHERE id = 'source-a'`, "alpha stays. The "+mergedSourceSecret+" words.", RefinementCompleted); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`INSERT INTO embeddings(session_id, chunk_index, embedding, text_hash, model) VALUES('source-a', 0, X'01', 'hash', 'model')`); err != nil {
		t.Fatal(err)
	}
	if err := store.StoreEvent("live_transcript", `{"type":"live_transcript","session_id":"source-a","text":"the `+mergedSourceSecret+` words","start_time":2,"end_time":4}`); err != nil {
		t.Fatal(err)
	}
	if err := store.StoreEvent("summary_ready", `{"type":"summary_ready","session_id":"source-a","summary":"The summary of `+mergedSourceSecret+`."}`); err != nil {
		t.Fatal(err)
	}

	if err := store.MergeSessions("merged", []string{"source-a", "source-b"}, startA, startB.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Canonicalize("merged"); err != nil {
		t.Fatal(err)
	}
	segments, err := store.GetSegments("merged")
	if err != nil {
		t.Fatal(err)
	}
	var secretID int64
	for _, segment := range segments {
		if strings.Contains(segment.Text, mergedSourceSecret) {
			secretID = segment.ID
		}
	}
	if err := store.DeleteSegment("merged", secretID, DeletedSpan{AudioPath: "recording-source-a.mp3", Start: 2, End: 4, SHA256Before: "before", SHA256After: "after"}); err != nil {
		t.Fatalf("DeleteSegment: %v", err)
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
