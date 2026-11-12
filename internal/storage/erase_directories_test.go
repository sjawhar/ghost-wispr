package storage

import (
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// TestRemovingARowKeepsItsRecordingDirectory fails if deleting a session
// (the session delete route, GC's age policy) or marking its audio removed
// (GC's disk pressure) drops the directory of a recording kept outside the
// audio directory before the recording is erased: an erase cut off there by a
// stop would then never be found as the app starts. Once the startup cleanup
// forgets the directory, it is no longer listed.
func TestRemovingARowKeepsItsRecordingDirectory(t *testing.T) {
	for name, remove := range map[string]func(*SQLiteStore, string) error{
		"DeleteSession": func(store *SQLiteStore, _ string) error {
			return store.DeleteSession("elsewhere")
		},
		"DeleteEndedSession": func(store *SQLiteStore, recording string) error {
			deleted, err := store.DeleteEndedSession("elsewhere", recording, false)
			if err == nil && !deleted {
				err = errors.New("the session was not deleted")
			}
			return err
		},
		"MarkAudioRemoved": func(store *SQLiteStore, recording string) error {
			_, marked, err := store.MarkAudioRemoved("elsewhere", recording, "", false, false, time.Now())
			if err == nil && !marked {
				err = errors.New("the audio was not marked removed")
			}
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			store := newTestSQLiteStore(t)
			dir := t.TempDir()
			recording := filepath.Join(dir, "elsewhere.mp3")
			started := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
			if err := store.CreateSession("elsewhere", started); err != nil {
				t.Fatal(err)
			}
			if err := store.EndSession("elsewhere", started.Add(time.Minute), recording); err != nil {
				t.Fatal(err)
			}
			listed := func() bool {
				t.Helper()
				dirs, err := store.RecordingDirectories()
				if err != nil {
					t.Fatal(err)
				}
				return slices.Contains(dirs, dir)
			}
			if !listed() {
				t.Fatal("control: the directory of the session's recording is not listed")
			}

			if err := remove(store, recording); err != nil {
				t.Fatal(err)
			}
			if !listed() {
				t.Errorf("the directory of the recording %s removes is not listed once its row is gone", name)
			}
			if err := store.ForgetEraseDirectories([]string{dir}); err != nil {
				t.Fatal(err)
			}
			if listed() {
				t.Error("the directory is still listed after the startup cleanup forgot it")
			}
		})
	}
}
