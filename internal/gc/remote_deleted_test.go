package gc

import (
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// remoteDeleted makes session id's state the one a sync leaves when the
// session's Drive folder is in the trash or gone.
func remoteDeleted(t *testing.T, store *storage.SQLiteStore, id string) {
	t.Helper()
	if err := store.UpdateSummary(id, "Standup", "A summary.", storage.SummaryCompleted, "default", "{}"); err != nil {
		t.Fatalf("summarize %s: %v", id, err)
	}
	syncTo(t, store, id, storage.SyncStateRemoteDeleted, "folder-"+id, 0, "the Drive folder is in the trash")
}

// A session whose Drive folder is in the trash or gone has its only lasting
// copy on the device (Drive empties its trash after 30 days): no GC path
// deletes it or removes its audio.
func TestGCNeverDeletesASessionWhoseDriveFolderIsGone(t *testing.T) {
	for _, tc := range []struct {
		name    string
		config  Config
		syncOff bool
	}{
		{"age policy, sync off", Config{MaxAgeDays: 30}, true},
		{"audio budget, sync off", Config{MaxAudioSizeMB: 1}, true},
		{"age policy, sync on", Config{MaxAgeDays: 30}, false},
		{"audio budget, sync on", Config{MaxAudioSizeMB: 1}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newStore(t)
			dir := t.TempDir()
			const id = "20260901100000"
			saved := endWithRecording(t, store, dir, id, daysAgo(60), 2*1024*1024)
			remoteDeleted(t, store, id)

			tc.config.AudioDir = dir
			var deleted []string
			if tc.syncOff {
				deleted = runGCWithSyncOff(t, store, tc.config, 2)
			} else {
				deleted = runGC(t, store, tc.config, 2)
			}

			if len(deleted) != 0 {
				t.Errorf("GC deleted %v", deleted)
			}
			expectEveryRowAndTranscript(t, store, id)
			expectAudioKept(t, store, id, saved)
		})
	}
}

// runGCWithSyncOff runs passes as the app does with Drive sync off: no Drive
// to ask, so every pass must hold with ErrDriveSyncOff.
func runGCWithSyncOff(t *testing.T, store *storage.SQLiteStore, config Config, passes int) []string {
	t.Helper()
	config.Drive = nil
	config.HoldReason = ErrDriveSyncOff
	var deleted []string
	for range passes {
		collector := New(store, config)
		ids, err := collector.Run()
		if err != nil {
			t.Fatalf("gc run: %v", err)
		}
		if !errors.Is(collector.Held(), ErrDriveSyncOff) {
			t.Errorf("sync-off pass held %v, want %v", collector.Held(), ErrDriveSyncOff)
		}
		deleted = append(deleted, ids...)
	}
	return deleted
}

// A GC pass that cannot check Drive deletes nothing and removes no audio, and
// says why (Held): no syncer runs, or Drive does not answer, which stops the
// whole pass at the first session it asks about.
func TestAGCPassThatCannotCheckDriveHoldsBack(t *testing.T) {
	for _, tc := range []struct {
		name  string
		drive *driveFolders
		want  error
	}{
		{"no syncer running", nil, ErrDriveNotRunning},
		{"Drive not answering", &driveFolders{err: errors.New("request timed out")}, ErrDriveCannotSay},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newStore(t)
			dir := t.TempDir()
			ids := []string{"20260901100000", "20260901110000"}
			var saved []string
			for i, id := range ids {
				saved = append(saved, endWithRecording(t, store, dir, id, daysAgo(60).Add(time.Duration(i)*time.Hour), 2*1024*1024))
				if err := store.UpdateSummary(id, "Standup", "A summary.", storage.SummaryCompleted, "default", "{}"); err != nil {
					t.Fatalf("summarize: %v", err)
				}
				syncTo(t, store, id, storage.SyncStateSynced, "folder-"+id, 0, "")
			}
			config := Config{MaxAgeDays: 30, MaxAudioSizeMB: 1, AudioDir: dir}
			if tc.drive != nil {
				config.Drive = tc.drive
			}
			collector := New(store, config)
			deleted, err := collector.Run()
			if err != nil {
				t.Fatalf("gc run: %v", err)
			}
			if len(deleted) != 0 {
				t.Errorf("deleted %v", deleted)
			}
			for i, id := range ids {
				expectAudioKept(t, store, id, saved[i])
			}
			if held := collector.Held(); !errors.Is(held, tc.want) {
				t.Errorf("Held() = %v, want %v", held, tc.want)
			}
			if tc.drive != nil && len(tc.drive.asked) != 1 {
				t.Errorf("asked Drive about %v, want the first session once: the pass stops there", tc.drive.asked)
			}
		})
	}
}

// A session GC keeps because Drive no longer has its folder stops counting
// against the audio budget, so GC does not remove the audio of other
// sessions to free space the kept one still takes.
func TestAKeptSessionsAudioStopsCountingAgainstTheBudget(t *testing.T) {
	store := newStore(t)
	dir := t.TempDir()
	const gone, present = "20260901100000", "20260901110000"
	goneSaved := endWithRecording(t, store, dir, gone, daysAgo(60), 2*1024*1024)
	presentSaved := endWithRecording(t, store, dir, present, daysAgo(59), 256*1024)
	for _, id := range []string{gone, present} {
		if err := store.UpdateSummary(id, "Standup", "A summary.", storage.SummaryCompleted, "default", "{}"); err != nil {
			t.Fatalf("summarize: %v", err)
		}
		syncTo(t, store, id, storage.SyncStateSynced, "folder-"+id, 0, "")
	}
	drive := &driveFolders{gone: map[string]bool{"folder-" + gone: true}}

	runGC(t, store, Config{MaxAudioSizeMB: 1, AudioDir: dir, Drive: drive}, 1)

	expectAudioKept(t, store, gone, goneSaved)
	expectAudioKept(t, store, present, presentSaved)
}

// A synced session that records no Drive folder cannot be checked on Drive:
// GC keeps it and its audio, and asks Drive nothing about it.
func TestGCKeepsASyncedSessionWithNoDriveFolder(t *testing.T) {
	store := newStore(t)
	dir := t.TempDir()
	const id = "20260901100000"
	saved := endWithRecording(t, store, dir, id, daysAgo(60), 2*1024*1024)
	if err := store.UpdateSummary(id, "Standup", "A summary.", storage.SummaryCompleted, "default", "{}"); err != nil {
		t.Fatalf("summarize: %v", err)
	}
	syncTo(t, store, id, storage.SyncStateSynced, "", 0, "")
	drive := &driveFolders{}

	if deleted := runGC(t, store, Config{MaxAgeDays: 30, MaxAudioSizeMB: 1, AudioDir: dir, Drive: drive}, 1); len(deleted) != 0 {
		t.Errorf("GC deleted %v", deleted)
	}
	expectAudioKept(t, store, id, saved)
	if len(drive.asked) != 0 {
		t.Errorf("asked Drive about %v, want nothing: the session names no folder", drive.asked)
	}
}

// A session synced by an earlier version, its documents unverified on Drive,
// is never deleted for its age. Its audio is on Drive: the audio budget
// removes it once Drive answers that the folder is there, and keeps it when
// the folder is in the trash or gone.
func TestAnUnverifiedSessionsAudioGoesOnlyOnceDriveHasItsFolder(t *testing.T) {
	for _, tc := range []struct {
		name    string
		gone    bool
		syncOff bool
		removed bool
	}{
		{"folder on Drive", false, false, true},
		{"folder gone", true, false, false},
		{"sync off", false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newStore(t)
			dir := t.TempDir()
			const id = "20260901100000"
			saved := endWithRecording(t, store, dir, id, daysAgo(60), 2*1024*1024)
			if err := store.UpdateSummary(id, "Standup", "A summary.", storage.SummaryCompleted, "default", "{}"); err != nil {
				t.Fatalf("summarize: %v", err)
			}
			syncTo(t, store, id, storage.SyncStateSynced, "folder-"+id, 0, "")
			if _, err := store.DB().Exec(`UPDATE sessions SET documents_unverified = 1 WHERE id = ?`, id); err != nil {
				t.Fatalf("mark unverified: %v", err)
			}
			config := Config{
				MaxAgeDays:     30,
				MaxAudioSizeMB: 1,
				AudioDir:       dir,
				Drive:          &driveFolders{gone: map[string]bool{"folder-" + id: tc.gone}},
			}

			var deleted []string
			if tc.syncOff {
				deleted = runGCWithSyncOff(t, store, config, 1)
			} else {
				deleted = runGC(t, store, config, 1)
			}
			if len(deleted) != 0 {
				t.Errorf("GC deleted %v: the documents are unverified", deleted)
			}
			expectEveryRowAndTranscript(t, store, id)
			if tc.removed {
				expectAudioRemoved(t, store, id, saved)
			} else {
				expectAudioKept(t, store, id, saved)
			}
		})
	}
}
