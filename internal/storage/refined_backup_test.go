package storage

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestDeleteRefinedSegmentScrubsOlderBackupSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "older.db")
	openWithSecret(t, path)
	older, err := sql.Open("sqlite", path+"?_pragma=secure_delete(1)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := older.Exec(`ALTER TABLE sessions DROP COLUMN drive_replace_error`); err != nil {
		t.Fatal(err)
	}
	if err := older.Close(); err != nil {
		t.Fatal(err)
	}
	store := openStoreWithBackup(t, path)
	session, err := store.GetSession("backed-up")
	if err != nil {
		t.Fatal(err)
	}
	words := []RefinedWord{
		{RecordingPath: "recording.mp3", RecordingSHA256: "hash", Ordinal: 0, Text: "before bridge.", Start: 0, End: 1},
		{RecordingPath: "recording.mp3", RecordingSHA256: "hash", Ordinal: 1, Text: backupSecret + ".", Start: 2, End: 3},
		{RecordingPath: "recording.mp3", RecordingSHA256: "hash", Ordinal: 2, Text: "after bridge.", Start: 4, End: 5},
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET refinement_generation = deletion_generation, status = 'ended' WHERE id = ?`, session.ID); err != nil {
		t.Fatal(err)
	}
	generation, err := store.DeletionGeneration(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	revision, err := store.PublishRefinementAtGeneration(session.ID, RefinedTranscript(words), "nova-3", words, BuildRefinedSegments(words), generation)
	if err != nil {
		t.Fatal(err)
	}
	segments, err := store.GetSegments(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	for i := range segments {
		if segments[i].Text == backupSecret+"." {
			if err := store.DeleteRefinedSegmentText(session.ID, segments[i].ID, revision); err != nil {
				t.Fatal(err)
			}
		}
	}
	assertBackupHolds(t, path, backupSecret, false)
}
