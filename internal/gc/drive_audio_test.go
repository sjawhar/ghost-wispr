package gc

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// A folder alone is not a durable copy: Drive Manager can remove a recording
// while retaining its session folder. Both GC paths therefore require Drive's
// checksum for every local recording before removing it.
func TestGCRemovesRecordingsOnlyAfterDriveContentVerification(t *testing.T) {
	content := bytes.Repeat([]byte("recording bytes"), 200000)
	sum := md5.Sum(content)
	matchingChecksum := hex.EncodeToString(sum[:])

	policies := []struct {
		name     string
		started  time.Time
		config   Config
		ageBased bool
	}{
		{
			name:     "age policy",
			started:  time.Now().UTC().AddDate(0, 0, -40),
			config:   Config{MaxAgeDays: 30},
			ageBased: true,
		},
		{
			name:    "audio budget",
			started: time.Now().UTC().Add(-time.Hour),
			config:  Config{MaxAudioSizeMB: 1},
		},
	}
	checks := []struct {
		name      string
		checksums map[string]string
		removed   bool
	}{
		{"folder exists, recording absent", map[string]string{}, false},
		{"recording has a different checksum", map[string]string{"audio-recording.mp3": "00000000000000000000000000000000"}, false},
		{"recording has the matching checksum", map[string]string{"audio-recording.mp3": matchingChecksum}, true},
	}

	for _, policy := range policies {
		for _, check := range checks {
			t.Run(policy.name+"/"+check.name, func(t *testing.T) {
				store := newStore(t)
				dir := t.TempDir()
				const id = "20260901120000"
				recording := filepath.Join(dir, "recording.mp3")
				if err := os.WriteFile(recording, content, 0o600); err != nil {
					t.Fatalf("write recording: %v", err)
				}
				createSession(t, store, id, policy.started)
				if err := store.EndSession(id, policy.started.Add(time.Minute), recording); err != nil {
					t.Fatalf("end session: %v", err)
				}
				if err := store.UpdateSummaryAtGeneration(id, "", "A summary.", storage.SummaryCompleted, "default", "{}", "", 0); err != nil {
					t.Fatalf("summarize: %v", err)
				}
				syncTo(t, store, id, storage.SyncStateSynced, "folder-1", 0, "")

				config := policy.config
				config.AudioDir = dir
				config.Drive = &driveFolders{recordingChecksums: check.checksums}
				deleted, err := New(store, config).Run()
				if err != nil {
					t.Fatalf("gc run: %v", err)
				}

				_, statErr := os.Stat(recording)
				if check.removed {
					if !os.IsNotExist(statErr) {
						t.Errorf("recording remains after a matching Drive checksum: %v", statErr)
					}
					if policy.ageBased && len(deleted) != 1 {
						t.Errorf("age policy deleted %v, want the session", deleted)
					}
					return
				}
				if statErr != nil {
					t.Errorf("recording disappeared without a matching Drive checksum: %v", statErr)
				}
				if _, err := store.GetSession(id); err != nil {
					t.Errorf("GC removed the session without a matching Drive checksum: %v", err)
				}
			})
		}
	}
}
