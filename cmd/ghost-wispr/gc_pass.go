package main

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/gc"
	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// gcHealth is whether the last GC pass held back, and how many sessions GC
// keeps until a sync verifies their documents on Drive, for GET /api/status
// and the web UI's warnings: a pass that cannot check Drive deletes nothing
// and removes no audio, and must not do so silently while the disk fills.
type gcHealth struct {
	mu         sync.Mutex
	hold       *server.GCHold
	unverified int
}

// gcStore is the store a GC pass runs on, and counts the sessions it keeps
// until a sync verifies them.
type gcStore interface {
	gc.Store
	CountUnverifiedSessions() (int, error)
}

// Hold returns the last pass's hold, nil when it did not hold back.
func (h *gcHealth) Hold() *server.GCHold {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.hold == nil {
		return nil
	}
	hold := *h.hold
	return &hold
}

// Unverified returns how many sessions GC keeps until a sync verifies their
// documents on Drive, as last counted.
func (h *gcHealth) Unverified() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.unverified
}

// countUnverified counts again the sessions GC keeps until a sync verifies
// them; a count that fails leaves the last one.
func (h *gcHealth) countUnverified(store gcStore) {
	n, err := store.CountUnverifiedSessions()
	if err != nil {
		slog.Warn("gc could not count the sessions it keeps until Drive is re-checked", "operation", "gc_health", "error", err)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.unverified = n
}

// record keeps why a pass at now held back (held, nil when it did not), with
// the disk use of dir's filesystem then. A hold that goes on keeps the time
// it began.
func (h *gcHealth) record(held error, dir string, now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if held == nil {
		h.hold = nil
		return
	}
	since := now
	if h.hold != nil {
		since = h.hold.Since
	}
	used, total, err := diskUse(dir)
	if err != nil {
		slog.Warn("gc could not read the disk use", "operation", "gc_health", "dir", dir, "error", err)
	}
	h.hold = &server.GCHold{Reason: held.Error(), Since: since, DiskUsedBytes: used, DiskTotalBytes: total}
}

// diskUse returns the bytes used on dir's filesystem and the bytes the app
// can have (used plus what is free to it, root's reserve left out), so
// used/total is how full the disk is, as df's Use% counts it.
func diskUse(dir string) (used, total int64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, 0, err
	}
	block := int64(st.Bsize)
	used = (int64(st.Blocks) - int64(st.Bfree)) * block
	return used, used + int64(st.Bavail)*block, nil
}

const recordingMismatchWarningPrefix = "recording-checksum-"

// clearStaleRecordingMismatchWarnings removes warnings for sessions that a
// later API or Drive action deleted, merged, or marked REMOTE_DELETED before
// this pass started.

func clearStaleRecordingMismatchWarnings(store gcStore, warnings *warningsRegistry, logger *slog.Logger) {
	if warnings == nil {
		return
	}
	for _, key := range warnings.KeysWithPrefix(recordingMismatchWarningPrefix) {
		sessionID := strings.TrimPrefix(key, recordingMismatchWarningPrefix)
		warnings.WithRecordingMismatch(func() {
			session, err := store.GetSession(sessionID)
			switch {
			case errors.Is(err, sql.ErrNoRows), errors.Is(err, os.ErrNotExist):
				warnings.Clear(key)
			case err != nil:
				logger.Warn("gc could not check whether a recording mismatch warning is stale", "operation", "clear_recording_mismatch_warning", "session_id", sessionID, "error", err)
			case session.Status == storage.SessionMerged, session.SyncState == storage.SyncStateRemoteDeleted:
				warnings.Clear(key)
			}
		})
	}
}

// runGCPass runs one GC pass under cfg, with drive the running Drive syncer
// (nil when none runs), and records in health whether it held back and how
// many sessions it keeps until a sync verifies them. A pass that failed (the
// store did not answer) says nothing about Drive: the last hold, if any,
// stays.
func runGCPass(store gcStore, cfg *config.Config, drive sessionSyncer, health *gcHealth, warnings *warningsRegistry, logger *slog.Logger) ([]string, error) {
	gcConfig := gcConfig(cfg, drive)
	if warnings != nil {
		gcConfig.OnRecordingMismatch = func(sessionID string, mismatch bool) {
			key := recordingMismatchWarningPrefix + sessionID
			warnings.WithRecordingMismatch(func() {
				if !mismatch {
					warnings.Clear(key)
					return
				}
				session, err := store.GetSession(sessionID)
				switch {
				case errors.Is(err, sql.ErrNoRows), errors.Is(err, os.ErrNotExist):
					warnings.Clear(key)
				case err != nil:
					logger.Warn("gc could not check whether a recording mismatch warning is current", "operation", "recording_mismatch", "session_id", sessionID, "error", err)
				case session.Status == storage.SessionMerged, session.SyncState == storage.SyncStateRemoteDeleted:
					warnings.Clear(key)
				default:
					warnings.Set(key, fmt.Sprintf("Recording for session %s does not match its Google Drive copy; local audio is retained.", sessionID))
				}
			})
		}
	}
	clearStaleRecordingMismatchWarnings(store, warnings, logger)
	collector := gc.New(store, gcConfig, logger)
	deleted, err := collector.Run()
	if err != nil {
		return nil, err
	}
	health.record(collector.Held(), cfg.AudioDir, time.Now())
	health.countUnverified(store)
	return deleted, nil
}
