package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// unansweringSyncer is a Drive syncer whose Drive does not answer.
type unansweringSyncer struct{ countingSyncer }

func (*unansweringSyncer) FolderOnDrive(context.Context, string) (bool, error) {
	return false, errors.New("drive: request timed out")
}

// A GC pass that cannot check Drive (sync on but not running, or Drive not
// answering) is recorded with the disk use, for the status API and the web
// UI, and a hold that goes on keeps the time it began; a pass that does not
// hold back clears it.
func TestAGCPassThatCannotCheckDriveIsRecordedWithTheDiskUse(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	const id = "20261001120000"
	started := time.Now().UTC().Add(-40 * 24 * time.Hour)
	if err := store.CreateSession(id, started); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.EndSession(id, started.Add(time.Minute), ""); err != nil {
		t.Fatalf("end: %v", err)
	}
	if err := store.UpdateSummary(id, "", "A summary.", storage.SummaryCompleted, "default", "{}"); err != nil {
		t.Fatalf("summarize: %v", err)
	}
	syncTo(t, store, id, storage.SyncStateSynced, "folder-1", 0, "")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Config{GCEnabled: true, GCMaxAgeDays: 30, GDriveSyncEnabled: true, AudioDir: t.TempDir()}
	health := &gcHealth{}

	if _, err := runGCPass(store, &cfg, nil, health, logger); err != nil {
		t.Fatalf("gc pass: %v", err)
	}
	hold := health.Hold()
	if hold == nil {
		t.Fatal("a pass with Drive sync on and no syncer running left no hold")
		return
	}
	if !strings.Contains(hold.Reason, "not running") || hold.DiskTotalBytes <= 0 || hold.DiskUsedBytes <= 0 || hold.DiskUsedBytes > hold.DiskTotalBytes {
		t.Errorf("hold = %+v, want why and the disk use", *hold)
	}
	began := hold.Since

	if _, err := runGCPass(store, &cfg, &unansweringSyncer{}, health, logger); err != nil {
		t.Fatalf("gc pass: %v", err)
	}
	if hold = health.Hold(); hold == nil || !strings.Contains(hold.Reason, "request timed out") || !hold.Since.Equal(began) {
		t.Errorf("after a pass Drive did not answer: hold %+v, want Drive's error since %v", hold, began)
	}
	if _, err := store.GetSession(id); err != nil {
		t.Errorf("a pass that could not check Drive deleted the session: %v", err)
	}

	// The session's documents are unverified on Drive (synced by an earlier
	// version): a pass that can ask Drive clears the hold, keeps it, and
	// counts it.
	if _, err := store.DB().Exec(`UPDATE sessions SET documents_unverified = 1 WHERE id = ?`, id); err != nil {
		t.Fatalf("mark unverified: %v", err)
	}
	if _, err := runGCPass(store, &cfg, &countingSyncer{}, health, logger); err != nil {
		t.Fatalf("gc pass: %v", err)
	}
	if hold = health.Hold(); hold != nil {
		t.Errorf("after a pass that could check Drive: hold %+v, want none", *hold)
	}
	if n := health.Unverified(); n != 1 {
		t.Errorf("Unverified() = %d, want 1", n)
	}
}
