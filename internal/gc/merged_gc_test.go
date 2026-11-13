package gc

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

func TestGCDeletesMergedSourcesAndTheirRawRecordings(t *testing.T) {
	dir := t.TempDir()
	store := newStore(t)
	started := time.Now().UTC().Add(-40 * 24 * time.Hour)
	for i, id := range []string{"source-a", "source-b"} {
		at := started.Add(time.Duration(i) * time.Hour)
		endWithRecording(t, store, dir, id, at, 1024)
	}
	if err := store.MergeSessions("merged", []string{"source-a", "source-b"}, started, started.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateSummaryAtGeneration("merged", "", "summary", storage.SummaryCompleted, "default", "{}", "", 0); err != nil {
		t.Fatal(err)
	}
	held := map[string]*os.File{}
	for _, id := range []string{"source-a", "source-b"} {
		raw := filepath.Join(dir, id+".pcm")
		contents := []byte("raw recording for " + id)
		if err := os.WriteFile(raw, contents, 0o600); err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(raw)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = f.Close() })
		held[id] = f
	}

	deleted := runGC(t, store, Config{MaxAgeDays: 30, AudioDir: dir}, 1)
	if len(deleted) != 1 || deleted[0] != "merged" {
		t.Fatalf("GC deleted %v, want [merged]", deleted)
	}
	for id, f := range held {
		if _, err := store.GetSession(id); err == nil {
			t.Errorf("merged source %s remains after GC", id)
		}
		raw := filepath.Join(dir, id+".pcm")
		if _, err := os.Stat(raw); !os.IsNotExist(err) {
			t.Errorf("raw recording %s after GC = %v, want removed", id, err)
		}
		got := make([]byte, len("raw recording for "+id))
		if _, err := f.ReadAt(got, 0); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, make([]byte, len(got))) {
			t.Errorf("raw recording %s after GC = %q, want zeroed", id, got)
		}
	}
}

func TestGCRefusesMergedSessionWithAnUnsafeSource(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*storage.SQLiteStore) error
	}{
		{
			name: "active",
			set: func(store *storage.SQLiteStore) error {
				_, err := store.DB().Exec(`UPDATE sessions SET status = ? WHERE id = 'source-a'`, storage.SessionActive)
				return err
			},
		},
		{
			name: "replacement pending",
			set: func(store *storage.SQLiteStore) error {
				_, err := store.DB().Exec(`UPDATE sessions SET drive_replace_pending = 1 WHERE id = 'source-a'`)
				return err
			},
		},
		{
			name: "folder owed",
			set: func(store *storage.SQLiteStore) error {
				_, err := store.DB().Exec(`INSERT INTO drive_replacement_folders(session_id, generation, folder_session_id, state)
					VALUES('source-a', 1, 'source-a', ?)`, storage.DriveReplacementFolderOwed)
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			store := newStore(t)
			started := time.Now().UTC().Add(-40 * 24 * time.Hour)
			for i, id := range []string{"source-a", "source-b"} {
				endWithRecording(t, store, dir, id, started.Add(time.Duration(i)*time.Hour), 1024)
			}
			if err := store.MergeSessions("merged", []string{"source-a", "source-b"}, started, started.Add(2*time.Hour)); err != nil {
				t.Fatal(err)
			}
			if err := store.UpdateSummaryAtGeneration("merged", "", "summary", storage.SummaryCompleted, "default", "{}", "", 0); err != nil {
				t.Fatal(err)
			}
			if err := tc.set(store); err != nil {
				t.Fatal(err)
			}

			if deleted := runGC(t, store, Config{MaxAgeDays: 30, AudioDir: dir}, 1); len(deleted) != 0 {
				t.Fatalf("GC deleted %v with an unsafe source, want none", deleted)
			}
			for _, id := range []string{"source-a", "source-b", "merged"} {
				if _, err := store.GetSession(id); err != nil {
					t.Errorf("GC removed %s despite unsafe source: %v", id, err)
				}
			}
		})
	}
}
