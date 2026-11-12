package gdrive

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/recording"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// TestSyncUploadsARecordingMovedWithTheAudioDir fails if a sync looks for a
// session's recording only where its entry points: after audio_dir changes
// and the recordings move with it, the recording is in the new audio
// directory (recording.Resolve), and the session would never sync, each try
// reporting the recording not on disk.
func TestSyncUploadsARecordingMovedWithTheAudioDir(t *testing.T) {
	store := openStore(t)
	const id = "20261106100000"
	oldDir, newDir := t.TempDir(), t.TempDir()
	summarizedSession(t, store, oldDir, id, time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC), "The summary.")
	entry := filepath.Join(oldDir, id+".mp3")
	moved := filepath.Join(newDir, id+".mp3")
	if err := os.Rename(entry, moved); err != nil {
		t.Fatal(err)
	}
	drive := newFakeDrive()
	o := NewOrchestratorWithUploader(drive, store)
	o.SetAudioDir(newDir)

	if err := o.SyncSession(context.Background(), id); err != nil {
		t.Fatalf("sync: %v", err)
	}
	state, folder := sessionState(t, store, id)
	if state != storage.SyncStateSynced {
		t.Errorf("sync state %q, want %q", state, storage.SyncStateSynced)
	}
	if got := drive.files(folder)[recording.FileName(entry)]; string(got) != "audio of "+id {
		t.Errorf("Drive holds %s = %q, want the recording moved to the new audio directory", recording.FileName(entry), got)
	}
}
