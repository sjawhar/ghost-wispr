package gc

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

func mergedGCFolderStore(t *testing.T) (*storage.SQLiteStore, string, time.Time) {
	t.Helper()
	dir := t.TempDir()
	store := newStore(t)
	started := time.Now().UTC().Add(-40 * 24 * time.Hour)
	for i, id := range []string{"source-a", "source-b"} {
		endWithRecording(t, store, dir, id, started.Add(time.Duration(i)*time.Hour), 1024)
		syncTo(t, store, id, storage.SyncStateSynced, "source-folder-"+id, 0, "")
	}
	if err := store.MergeSessions("merged", []string{"source-a", "source-b"}, started, started.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateSummaryAtGeneration("merged", "", "summary", storage.SummaryCompleted, "default", "{}", "", 0); err != nil {
		t.Fatal(err)
	}
	return store, dir, started
}

func TestGCAgesMergedSourcesAfterTheirTextDocumentsRefresh(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, store *storage.SQLiteStore, dir string)
	}{
		{
			name: "delete text and refresh source folders",
			change: func(t *testing.T, store *storage.SQLiteStore, dir string) {
				segments, err := store.GetSegments("merged")
				if err != nil {
					t.Fatal(err)
				}
				if err := store.DeleteSegmentText("merged", segments[0].ID, storage.TextDeletionSpan{AudioPath: filepath.Join(dir, "source-a.mp3"), Start: 0, End: 1, SHA256: "before"}); err != nil {
					t.Fatal(err)
				}
				for _, id := range []string{"source-a", "source-b", "merged"} {
					if cleared, err := store.ClearDriveDocumentsPending(id, func(storage.Session) bool { return true }); err != nil || !cleared {
						t.Fatalf("refresh source %s = %v, %v", id, cleared, err)
					}
				}
				if _, err := store.DB().Exec(`UPDATE sessions SET summary_status = ? WHERE id = 'merged'`, storage.SummaryCompleted); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "retitle source",
			change: func(t *testing.T, store *storage.SQLiteStore, _ string) {
				if err := store.UpdateTitle("source-a", "Retitled source"); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, dir, _ := mergedGCFolderStore(t)
			tc.change(t, store, dir)
			if deleted := runGC(t, store, Config{MaxAgeDays: 30, AudioDir: dir}, 1); len(deleted) != 1 || deleted[0] != "merged" {
				t.Fatalf("GC deleted %v after source document refresh, want [merged]", deleted)
			}
		})
	}
}

func TestGCKeepsMergedSessionWhenASourceFolderIsGone(t *testing.T) {
	store, dir, _ := mergedGCFolderStore(t)
	drive := &driveFolders{gone: map[string]bool{"source-folder-source-a": true}}
	if deleted := runGC(t, store, Config{MaxAgeDays: 30, AudioDir: dir, Drive: drive}, 1); len(deleted) != 0 {
		t.Fatalf("GC deleted %v with source folder gone, want none", deleted)
	}
	if _, err := store.GetSession("merged"); err != nil {
		t.Fatalf("merged session after source folder loss = %v, want kept", err)
	}
}

type sourceFolderErrorDrive struct {
	asked   []string
	failing string
}

func (d *sourceFolderErrorDrive) FolderOnDrive(_ context.Context, id string) (bool, error) {
	d.asked = append(d.asked, id)
	if id == d.failing {
		return false, errors.New("Drive unavailable")
	}
	return true, nil
}

func (*sourceFolderErrorDrive) RecordingsMatch(context.Context, string, map[string]string) (bool, error) {
	return true, nil
}

func TestGCStopsWhenASourceFolderCheckErrors(t *testing.T) {
	store, dir, _ := mergedGCFolderStore(t)
	syncTo(t, store, "merged", storage.SyncStateSynced, "merged-folder", 0, "")
	drive := &sourceFolderErrorDrive{failing: "source-folder-source-a"}
	collector := New(store, Config{MaxAgeDays: 30, AudioDir: dir, Drive: drive})
	deleted, err := collector.Run()
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 0 {
		t.Fatalf("GC deleted %v when source Drive check failed", deleted)
	}
	if !errors.Is(collector.Held(), ErrDriveCannotSay) {
		t.Fatalf("GC hold after source Drive error = %v, want %v", collector.Held(), ErrDriveCannotSay)
	}
}
