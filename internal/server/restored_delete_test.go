package server

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
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
					if err := deleteSessionErasingAudio(store, id, dir, func() {}, nil, nil, func([]string) {}); err != nil {
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

// Re-review of #76 at 99d961c, P2 (the reviewer's repro): a restored name is
// a Drive file name, so it is taken only as the name of a recording in the
// audio directory, never resolved against the working directory (which on
// the Pi holds .env and the binary), and a name with a comma, which an audio
// path would split, or that is not a recording's name, is not taken at all.
// The recording a valid name names is erased.
func TestADeleteTakesARestoredNameOnlyAsARecordingInTheAudioDirectory(t *testing.T) {
	work := t.TempDir()
	t.Chdir(work)
	target := filepath.Join(work, "zz-review-target.env")
	if err := os.WriteFile(target, []byte("SECRET=1"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	const id, valid = "20261106140000", "20261106150000.mp3"
	recording := filepath.Join(dir, valid)
	if err := os.WriteFile(recording, []byte("conversation"), 0o600); err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 11, 6, 14, 0, 0, 0, time.UTC)
	ended := started.Add(time.Hour)
	sess := storage.Session{ID: id, StartedAt: started, EndedAt: &ended, Status: storage.SessionEnded, SummaryStatus: storage.SummaryCompleted, RefinementStatus: storage.RefinementNoAudio}
	if err := store.ImportSession(&sess, nil, []string{"zz-review-target.env", "a,zz-review-target.env", valid}); err != nil {
		t.Fatal(err)
	}
	if named, err := store.NamedAudio(dir); err != nil || !slices.Equal(named, []string{recording}) {
		t.Errorf("NamedAudio = %q (%v), want only %s", named, err, recording)
	}

	if err := deleteSessionErasingAudio(store, id, dir, func() {}, nil, nil, func([]string) {}); err != nil {
		t.Fatalf("delete %s: %v", id, err)
	}

	if got, err := os.ReadFile(target); err != nil || string(got) != "SECRET=1" {
		t.Errorf("zz-review-target.env in the working directory holds %q (%v), want it as it was", got, err)
	}
	if _, err := os.Lstat(recording); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("control: %s, which the restored folder held, is still in the audio directory (%v)", valid, err)
	}
}
