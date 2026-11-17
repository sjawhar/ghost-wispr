package server

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// Re-review of #76 at 1a167b7, P1 (the reviewer's repro): on a restored unit
// a session A and a merged session M both have A.mp3 in their restored Drive
// folders (restored_recordings), and a Retry refinement on A names A.mp3 in
// A's audio path. Deleting A leaves A.mp3, since M will name it. Deleting M
// then erases A.mp3 too: a delete erases the recordings its rows' restored
// folders hold, which the rows' delete drops with them, except one a
// remaining session names. Without M, deleting A erases A.mp3 at once.
func TestDeletingTheLastSessionWhoseRestoredFolderHoldsARecordingErasesIt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		merged bool
	}{
		{name: "a merged session also holds it", merged: true},
		{name: "control: no other session holds it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			const a, m = "20261106140000", "20261106150000"
			recording := filepath.Join(dir, a+".mp3")
			if err := os.WriteFile(recording, []byte("conversation"), 0o600); err != nil {
				t.Fatal(err)
			}
			restore := func(id string) {
				t.Helper()
				started := time.Date(2026, 11, 6, 14, 0, 0, 0, time.UTC)
				ended := started.Add(time.Hour)
				sess := storage.Session{ID: id, StartedAt: started, EndedAt: &ended, Status: storage.SessionEnded, SummaryStatus: storage.SummaryCompleted, RefinementStatus: storage.RefinementNoAudio}
				if err := store.ImportSession(&sess, nil, []string{a + ".mp3"}); err != nil {
					t.Fatal(err)
				}
			}
			restore(a)
			if tc.merged {
				restore(m)
			}
			// Retry refinement on A downloaded A.mp3 and names it.
			if _, err := store.DB().Exec(`UPDATE sessions SET audio_path = ? WHERE id = ?`, recording, a); err != nil {
				t.Fatal(err)
			}
			deleteAll := func(ids ...string) {
				t.Helper()
				for _, id := range ids {
					if err := deleteSessionErasingAudio(store, id, dir, func() {}, nil, func([]string) {}); err != nil {
						t.Fatalf("delete %s: %v", id, err)
					}
				}
			}

			deleteAll(a)
			_, err = os.Lstat(recording)
			if tc.merged && err != nil {
				t.Fatalf("A.mp3, which the merged session's restored folder holds, is gone after deleting A: %v", err)
			}
			if tc.merged {
				deleteAll(m)
				_, err = os.Lstat(recording)
			}
			if !errors.Is(err, os.ErrNotExist) {
				t.Errorf("after deleting every session, A.mp3 is still on disk (%v)", err)
			}
			if records, err := store.DeletedSessionAudio(); err != nil || len(records) != 0 {
				t.Errorf("delete records left = %+v (%v), want none", records, err)
			}
		})
	}
}
