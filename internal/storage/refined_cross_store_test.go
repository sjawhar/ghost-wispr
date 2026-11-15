package storage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDeleteRefinedSegmentClearsEveryTextStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cross-store.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	if err := store.CreateSession("s", started); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("s", started.Add(time.Minute), "recording.mp3"); err != nil {
		t.Fatal(err)
	}
	const secret = "crossstoresentinel"
	words := []RefinedWord{
		{RecordingPath: "recording.mp3", RecordingSHA256: "hash", Ordinal: 0, Text: "Before.", Start: 0, End: 1},
		{RecordingPath: "recording.mp3", RecordingSHA256: "hash", Ordinal: 1, Text: secret + ".", Start: 2, End: 3},
		{RecordingPath: "recording.mp3", RecordingSHA256: "hash", Ordinal: 2, Text: "After.", Start: 4, End: 5},
	}
	revision, err := store.PublishRefinementAtGeneration("s", RefinedTranscript(words), "nova-3", words, BuildRefinedSegments(words), 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateSummaryAtGeneration("s", secret, secret, SummaryCompleted, "default", "{}", "", 0); err != nil {
		t.Fatal(err)
	}
	if err := store.StoreEmbedding("s", 0, []float32{1}, "hash", "model"); err != nil {
		t.Fatal(err)
	}
	if err := store.StoreEvent("live_transcript", `{"session_id":"s","type":"live_transcript","text":"crossstoresentinel","start_time":2,"end_time":3}`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	segments, err := store.GetSegments("s")
	if err != nil {
		t.Fatal(err)
	}
	for i := range segments {
		if strings.Contains(segments[i].Text, secret) {
			if err := store.DeleteRefinedSegmentText("s", segments[i].ID, revision); err != nil {
				t.Fatal(err)
			}
		}
	}
	session, err := store.GetSession("s")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(session.RefinedTranscript+session.CanonicalTranscript+session.Summary+session.Title, secret) {
		t.Fatalf("session text still holds deleted occurrence: %#v", session)
	}
	var embeddings, events, indexed int
	if err := store.DB().QueryRow(`SELECT count(*) FROM embeddings WHERE session_id = 's'`).Scan(&embeddings); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRow(`SELECT count(*) FROM mcp_events WHERE payload LIKE ?`, "%"+secret+"%").Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRow(`SELECT count(*) FROM sessions_fts WHERE sessions_fts MATCH ?`, secret).Scan(&indexed); err != nil {
		t.Fatal(err)
	}
	if embeddings != 0 || events != 0 || indexed != 0 {
		t.Fatalf("post-delete stores embeddings=%d events=%d search=%d, want zero", embeddings, events, indexed)
	}
	for _, candidate := range []string{path, path + "-wal"} {
		contents, err := os.ReadFile(candidate)
		if err == nil && strings.Contains(string(contents), secret) {
			t.Fatalf("%s still holds deleted text", candidate)
		}
	}
	backup, err := os.ReadFile(preMigrationBackupPath(path))
	if err == nil && strings.Contains(string(backup), secret) {
		t.Fatal("backup still holds deleted text")
	}
}
