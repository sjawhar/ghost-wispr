package storage

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestUntimedDeleteOwesMergedSourceDriveDocuments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "untimed-drive.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	if err := store.CreateSession("a", started); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSegment("a", transcribeSegment("private untimed words", 0, started)); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("a", started.Add(time.Minute), ""); err != nil {
		t.Fatal(err)
	}
	if err := store.StoreEvent("live_transcript", `{"type":"live_transcript","session_id":"a","text":"private untimed words","start_time":0,"end_time":1}`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`INSERT INTO embeddings(session_id, chunk_index, embedding, text_hash, model) VALUES('a', 0, X'00', 'hash', 'test')`); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession("b", started.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("b", started.Add(61*time.Minute), "/audio/b.mp3"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET gdrive_folder_id = 'folder-' || id, sync_status = ?, sync_state = ? WHERE id IN ('a', 'b')`, SyncSynced, SyncStateSynced); err != nil {
		t.Fatal(err)
	}
	if err := store.MergeSessions("merged", []string{"a", "b"}, started, started.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	words := []RefinedWord{{RecordingPath: "/audio/b.mp3", RecordingSHA256: "b", Ordinal: 0, Text: "timed words", Start: 0, End: 1}}
	if _, err := store.PublishRefinementAtGeneration("merged", RefinedTranscript(words), "test", words, BuildRefinedSegments(words), 0); err != nil {
		t.Fatal(err)
	}
	segments, err := store.GetSegments("merged")
	if err != nil {
		t.Fatal(err)
	}
	for _, segment := range segments {
		if strings.Contains(segment.Text, "private untimed") {
			if err := store.DeleteRefinedSegmentText("merged", segment.ID, 0); err != nil {
				t.Fatal(err)
			}
		}
	}
	var sourceSegments, sourceEvents, sourceEmbeddings, indexed int
	if err := store.DB().QueryRow(`SELECT count(*) FROM segments WHERE session_id = 'a'`).Scan(&sourceSegments); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRow(`SELECT count(*) FROM mcp_events WHERE payload LIKE '%private untimed%'`).Scan(&sourceEvents); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRow(`SELECT count(*) FROM embeddings WHERE session_id = 'a'`).Scan(&sourceEmbeddings); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRow(`SELECT count(*) FROM sessions_fts WHERE sessions_fts MATCH 'private'`).Scan(&indexed); err != nil {
		t.Fatal(err)
	}
	if sourceSegments != 0 || sourceEvents != 0 || sourceEmbeddings != 0 || indexed != 0 {
		t.Fatalf("untimed delete stores = segments:%d events:%d embeddings:%d indexed:%d", sourceSegments, sourceEvents, sourceEmbeddings, indexed)
	}
	assertDatabaseFilesLack(t, path, "private untimed words", "timed words")
	owed, err := store.DriveDocumentsOwed("merged")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(owed, "a") {
		t.Fatalf("owed Drive documents = %v, want source a", owed)
	}
}
