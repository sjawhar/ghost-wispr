package gc

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
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

func deleteRefinedTextForGCTest(t *testing.T, store *storage.SQLiteStore, sessionID string, liveSegmentID int64, span storage.TextDeletionSpan) error {
	t.Helper()
	live, err := store.GetSegments(sessionID)
	if err != nil {
		return err
	}
	index := -1
	words := make([]storage.RefinedWord, 0, len(live))
	refined := make([]storage.RefinedSegment, 0, len(live))
	for i := range live {
		segment := &live[i]
		if segment.ID == liveSegmentID {
			index = i
		}
		words = append(words, storage.RefinedWord{
			RecordingPath: span.AudioPath, RecordingSHA256: span.SHA256, Ordinal: i,
			Text: segment.Text, Speaker: segment.Speaker, Start: segment.StartTime, End: segment.EndTime,
		})
		refined = append(refined, storage.RefinedSegment{
			RecordingPath: span.AudioPath, RecordingSHA256: span.SHA256, FirstWordOrdinal: i, LastWordOrdinal: i,
			Text: segment.Text, Speaker: segment.Speaker, Start: segment.StartTime, End: segment.EndTime,
		})
	}
	if index < 0 {
		return storage.ErrSegmentNotFound
	}
	generation, err := store.DeletionGeneration(sessionID)
	if err != nil {
		return err
	}
	revision, err := store.PublishRefinementAtGeneration(sessionID, storage.RefinedTranscript(words), "test", words, refined, generation)
	if err != nil {
		return err
	}
	segments, err := store.GetSegments(sessionID)
	if err != nil {
		return err
	}
	return store.DeleteRefinedSegmentText(sessionID, segments[index].ID, revision)
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
				if err := deleteRefinedTextForGCTest(t, store, "merged", segments[0].ID, storage.TextDeletionSpan{AudioPath: filepath.Join(dir, "source-a.mp3"), Start: 0, End: 1, SHA256: "before"}); err != nil {
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

func TestGCKeepsMergedSessionWhileASourceRefreshIsOwed(t *testing.T) {
	store, dir, _ := mergedGCFolderStore(t)
	segments, err := store.GetSegments("merged")
	if err != nil {
		t.Fatal(err)
	}
	if err := deleteRefinedTextForGCTest(t, store, "merged", segments[0].ID, storage.TextDeletionSpan{AudioPath: filepath.Join(dir, "source-a.mp3"), Start: 0, End: 1, SHA256: "before"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET summary_status = ?, drive_documents_pending = 0 WHERE id = 'merged'`, storage.SummaryCompleted); err != nil {
		t.Fatal(err)
	}
	var pending int
	if err := store.DB().QueryRow(`SELECT drive_documents_pending FROM sessions WHERE id = 'source-a'`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Fatalf("source-a refresh mark = %d, want 1", pending)
	}
	if deleted := runGC(t, store, Config{MaxAgeDays: 30, AudioDir: dir}, 1); len(deleted) != 0 {
		t.Fatalf("GC deleted %v while source refresh is owed, want none", deleted)
	}
}

func TestGCRefusesAFolderlessRefiningSource(t *testing.T) {
	dir := t.TempDir()
	store := newStore(t)
	started := time.Now().UTC().Add(-40 * 24 * time.Hour)
	endWithRecording(t, store, dir, "source-a", started, 1024)
	syncTo(t, store, "source-a", storage.SyncStateSynced, "source-folder-a", 0, "")
	endWithRecording(t, store, dir, "source-b", started.Add(time.Hour), 1024)
	if _, err := store.DB().Exec(`UPDATE sessions SET refinement_status = ? WHERE id = 'source-b'`, storage.RefinementRunning); err != nil {
		t.Fatal(err)
	}
	if err := store.MergeSessions("merged", []string{"source-a", "source-b"}, started, started.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateSummaryAtGeneration("merged", "", "summary", storage.SummaryCompleted, "default", "{}", "", 0); err != nil {
		t.Fatal(err)
	}
	if deleted := runGC(t, store, Config{MaxAgeDays: 30, AudioDir: dir}, 1); len(deleted) != 0 {
		t.Fatalf("GC deleted %v with a folderless refining source, want none", deleted)
	}
}

func TestGCRefusesARemoteDeletedSource(t *testing.T) {
	store, dir, _ := mergedGCFolderStore(t)
	if _, err := store.DB().Exec(`UPDATE sessions SET sync_state = ?, sync_status = ? WHERE id = 'source-a'`, storage.SyncStateRemoteDeleted, storage.SyncFailed); err != nil {
		t.Fatal(err)
	}
	if deleted := runGC(t, store, Config{MaxAgeDays: 30, AudioDir: dir}, 1); len(deleted) != 0 {
		t.Fatalf("GC deleted %v with a remote-deleted source, want none", deleted)
	}
}

func TestGCLogsTheSourceThatBlocksAgeDeletion(t *testing.T) {
	store, dir, _ := mergedGCFolderStore(t)
	segments, err := store.GetSegments("merged")
	if err != nil {
		t.Fatal(err)
	}
	if err := deleteRefinedTextForGCTest(t, store, "merged", segments[0].ID, storage.TextDeletionSpan{AudioPath: filepath.Join(dir, "source-a.mp3"), Start: 0, End: 1, SHA256: "before"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET summary_status = ?, drive_documents_pending = 0 WHERE id = 'merged'`, storage.SummaryCompleted); err != nil {
		t.Fatal(err)
	}
	syncTo(t, store, "merged", storage.SyncStateSynced, "merged-folder", 0, "")
	var logs bytes.Buffer
	collector := New(store, Config{MaxAgeDays: 30, AudioDir: dir, Drive: &driveFolders{}}, slog.New(slog.NewJSONHandler(&logs, nil)))
	if deleted, err := collector.Run(); err != nil || len(deleted) != 0 {
		t.Fatalf("GC with an owed source refresh = %v, %v; want no deletion", deleted, err)
	}
	deleteWarnings := 0
	identified := false
	for _, line := range logLines(t, &logs) {
		if line["operation"] == "delete_session" && line["level"] == "WARN" {
			deleteWarnings++
		}
		if line["source_session_id"] == "source-a" && line["source_block_reason"] == "documents need Drive refresh" {
			identified = true
		}
	}
	if !identified || deleteWarnings != 1 {
		t.Fatalf("GC source block warnings = %d, identified=%v:\n%s", deleteWarnings, identified, logs.String())
	}
}

func TestGCReportsEveryMergedSourceBlockReason(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason string
		set    func(t *testing.T, store *storage.SQLiteStore, dir string)
	}{
		{
			name:   "active",
			reason: "active",
			set: func(t *testing.T, store *storage.SQLiteStore, _ string) {
				if _, err := store.DB().Exec(`UPDATE sessions SET status = ? WHERE id = 'source-a'`, storage.SessionActive); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:   "remote deleted",
			reason: "remote-deleted",
			set: func(t *testing.T, store *storage.SQLiteStore, _ string) {
				if _, err := store.DB().Exec(`UPDATE sessions SET sync_state = ?, sync_status = ? WHERE id = 'source-a'`, storage.SyncStateRemoteDeleted, storage.SyncFailed); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:   "refinement running",
			reason: "refinement running",
			set: func(t *testing.T, store *storage.SQLiteStore, _ string) {
				if _, err := store.DB().Exec(`UPDATE sessions SET refinement_status = ? WHERE id = 'source-a'`, storage.RefinementRunning); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:   "replacement owed",
			reason: "Drive replacement owed",
			set: func(t *testing.T, store *storage.SQLiteStore, _ string) {
				if _, err := store.DB().Exec(`UPDATE sessions SET drive_replace_pending = 1 WHERE id = 'source-a'`); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:   "folder replacement owed",
			reason: "Drive folder replacement owed",
			set: func(t *testing.T, store *storage.SQLiteStore, _ string) {
				if _, err := store.DB().Exec(`INSERT INTO drive_replacement_folders(session_id, generation, folder_session_id, state)
					VALUES('source-a', 1, 'source-a', ?)`, storage.DriveReplacementFolderOwed); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:   "document refresh owed",
			reason: "documents need Drive refresh",
			set: func(t *testing.T, store *storage.SQLiteStore, dir string) {
				segments, err := store.GetSegments("merged")
				if err != nil {
					t.Fatal(err)
				}
				if err := deleteRefinedTextForGCTest(t, store, "merged", segments[0].ID, storage.TextDeletionSpan{AudioPath: filepath.Join(dir, "source-a.mp3"), Start: 0, End: 1, SHA256: "before"}); err != nil {
					t.Fatal(err)
				}
				if _, err := store.DB().Exec(`UPDATE sessions SET summary_status = ?, drive_documents_pending = 0 WHERE id = 'merged'`, storage.SummaryCompleted); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, dir, _ := mergedGCFolderStore(t)
			tc.set(t, store, dir)
			syncTo(t, store, "merged", storage.SyncStateSynced, "merged-folder", 0, "")
			var logs bytes.Buffer
			collector := New(store, Config{MaxAgeDays: 30, AudioDir: dir, Drive: &driveFolders{}}, slog.New(slog.NewJSONHandler(&logs, nil)))
			if deleted, err := collector.Run(); err != nil || len(deleted) != 0 {
				t.Fatalf("GC with source %s = %v, %v; want no deletion", tc.name, deleted, err)
			}
			for _, line := range logLines(t, &logs) {
				if line["source_session_id"] == "source-a" && line["source_block_reason"] == tc.reason {
					return
				}
			}
			t.Fatalf("GC did not log source reason %q:\n%s", tc.reason, logs.String())
		})
	}
}

func TestGCKeepsMergedSessionWhileItsOwnRefreshIsOwed(t *testing.T) {
	store, dir, _ := mergedGCFolderStore(t)
	syncTo(t, store, "merged", storage.SyncStateSynced, "merged-folder", 0, "")
	if _, err := store.DB().Exec(`UPDATE sessions SET drive_documents_pending = 1 WHERE id = 'merged'`); err != nil {
		t.Fatal(err)
	}
	if deleted := runGC(t, store, Config{MaxAgeDays: 30, AudioDir: dir}, 1); len(deleted) != 0 {
		t.Fatalf("GC deleted %v while the merged session refresh is owed, want none", deleted)
	}
}
