package gdrive

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// TestSyncRetriesAnUnreadableRecordingWithBackoff fails if a recording on
// disk that cannot be read stops the session's other files from reaching
// Drive, fails the sync for good at once, or is retried on every sweep
// instead of with the sync's backoff, as a recording missing from disk is.
func TestSyncRetriesAnUnreadableRecordingWithBackoff(t *testing.T) {
	store := openStore(t)
	const id = "20261106190000"
	audioDir := t.TempDir()
	started := time.Date(2026, 11, 6, 19, 0, 0, 0, time.UTC)
	summarizedSession(t, store, audioDir, id, started, "summary")
	good := filepath.Join(audioDir, id+".mp3")
	unreadable := filepath.Join(audioDir, "unreadable.mp3")
	if err := os.Mkdir(unreadable, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET audio_path = ? WHERE id = ?`, good+","+unreadable, id); err != nil {
		t.Fatal(err)
	}
	wire, syncer := newWireDrive(t)
	o := NewOrchestrator(syncer, store)

	if err := o.SyncSession(context.Background(), id); err == nil {
		t.Fatal("sync succeeded despite an unreadable recording")
	}
	session, err := store.GetSession(id)
	if err != nil {
		t.Fatal(err)
	}
	if session.SyncState != storage.SyncStateRetryScheduled || session.RetryCount != 1 {
		t.Fatalf("after an unreadable recording: state=%q retry=%d, want %q with retry 1", session.SyncState, session.RetryCount, storage.SyncStateRetryScheduled)
	}
	if IsRetryReady(time.Now().UTC(), session.RetryCount, session.LastSyncAttempt) {
		t.Errorf("the retry is due at once; want it after the %s backoff", RetryDelayForCount(session.RetryCount))
	}
	if session.GDriveFolderID == "" {
		t.Fatal("readable session files did not reach Drive")
	}
	if copies := wire.in(session.GDriveFolderID)["audio-"+id+".mp3"]; len(copies) != 1 {
		t.Fatalf("readable recording copies = %d, want one", len(copies))
	}

	if err := os.Remove(unreadable); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unreadable, []byte("recovered audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := o.SyncSession(context.Background(), id); err != nil {
		t.Fatalf("sync after recording recovery: %v", err)
	}
	session, err = store.GetSession(id)
	if err != nil {
		t.Fatal(err)
	}
	if session.SyncState != storage.SyncStateSynced {
		t.Errorf("after recording recovery: state=%q, want SYNCED", session.SyncState)
	}
	if copies := wire.in(session.GDriveFolderID)["audio-unreadable.mp3"]; len(copies) != 1 {
		t.Errorf("recovered recording copies = %d, want one", len(copies))
	}
}
