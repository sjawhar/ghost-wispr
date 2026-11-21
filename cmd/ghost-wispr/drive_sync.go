package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"sync"
	"time"

	driveapi "google.golang.org/api/drive/v3"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/gdrive"
	"github.com/sjawhar/ghost-wispr/internal/googleauth"
	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// newDriveSyncer finds the Google credential Drive uses
// (google_credentials_file, else Application Default Credentials) and builds
// a Drive syncer on cfg's root folder. Its error says which step failed; each
// caller adds its own context.
func newDriveSyncer(ctx context.Context, cfg *config.Config, logger *slog.Logger) (*gdrive.Syncer, error) {
	credentials, err := googleauth.Detect(ctx, cfg.GoogleCredentialsFile, driveapi.DriveFileScope)
	if err != nil {
		return nil, fmt.Errorf("detect Google credentials: %w", err)
	}
	syncer, err := gdrive.NewSyncer(ctx, credentials, cfg.GDriveFolderID, logger)
	if err != nil {
		return nil, fmt.Errorf("create Drive syncer: %w", err)
	}
	return syncer, nil
}

// sessionSyncer syncs one session to Google Drive.
type sessionSyncer interface {
	// CanSync reports, at once, why a sync of the session cannot start, if
	// it cannot.
	CanSync(ctx context.Context, sessionID string) error
	SyncSession(ctx context.Context, sessionID string) error
	// FolderOnDrive reports whether Drive has a session folder out of the
	// trash; GC asks it before it deletes a session or removes its audio.
	FolderOnDrive(ctx context.Context, folderID string) (bool, error)
	// RecordingsMatch reports whether Drive has every supplied recording with
	// the matching MD5 checksum before GC removes local audio.
	RecordingsMatch(ctx context.Context, folderID string, checksums map[string]string) (bool, error)
}

// driveSync is the Drive syncer the app uses, nil while sync is off. Turning
// sync on or off in the settings swaps it while the sweep and the retry-sync
// hook use it, so it is read and written under a lock.
type driveSync struct {
	mu     sync.Mutex
	syncer sessionSyncer
}

func (d *driveSync) Set(s sessionSyncer) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.syncer = s
}

func (d *driveSync) Get() sessionSyncer {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.syncer
}

// retrySync starts a sync of session sessionID in the background, as work the
// stop cuts off and waits for (background.Go), with syncer, the running Drive
// syncer (nil while sync is off), and says at once if none can start (for a
// session whose Drive folder was deleted, while Drive says it is not back),
// so the API does not report a retry that does nothing. Drive not answering
// whether such a folder is back is server.ErrDriveUnavailable: the retry may
// work once Drive answers.
func retrySync(ctx context.Context, background backgroundWork, syncer sessionSyncer, sessionID string) error {
	if syncer == nil {
		return server.ErrSyncNotConfigured
	}
	if err := syncer.CanSync(ctx, sessionID); err != nil {
		if errors.Is(err, gdrive.ErrDriveDidNotAnswer) {
			return fmt.Errorf("%w: %w", server.ErrDriveUnavailable, err)
		}
		return err
	}
	background.Go(func(ctx context.Context) {
		if err := syncer.SyncSession(ctx, sessionID); err != nil {
			log.Printf("gdrive retry: session %s: %v", sessionID, err)
		}
	})
	return nil
}

// watchDriveSyncSettings keeps the running Drive syncer in line with the sync
// settings: started when sync is turned on (with a folder), stopped when it
// is turned off. It applies the latest config, one change at a time (see
// onConfigChange), so two changes saved together (sync on, then off) leave
// the syncer matching the last, however slow starting a syncer is. start
// makes a syncer; swapped runs with each syncer set (nil when sync stops).
func watchDriveSyncSettings(cfgStore *config.Store, drive *driveSync, start func(cfg *config.Config) (sessionSyncer, error), swapped func(sessionSyncer)) {
	onConfigChange(cfgStore, func(cfg *config.Config) {
		if cfg.GDriveSyncEnabled && cfg.GDriveFolderID != "" && drive.Get() == nil {
			syncer, err := start(cfg)
			if err != nil {
				slog.Warn("config: gdrive sync not started", "error", err)
				return
			}
			drive.Set(syncer)
			swapped(syncer)
			log.Printf("config: gdrive sync enabled")
		} else if !cfg.GDriveSyncEnabled && drive.Get() != nil {
			swapped(nil)
			drive.Set(nil)
			log.Printf("config: gdrive sync disabled")
		}
	})
}

// syncSweepStore lists the sessions the sweep syncs, and those that owe
// Drive a replacement, and retries the erase deletions still owe the
// write-ahead log and the pre-migration backup.
type syncSweepStore interface {
	GetSessionsNeedingSync() ([]string, error)
	GetSessionsBySyncState(syncState string) ([]storage.Session, error)
	RetryOwedErasure() error
	owedReplacementStore
}

// sweepDriveSync syncs, every interval until ctx ends, the sessions waiting
// for their first sync and those whose retry is due. While sync is off it
// syncs nothing, and a sweep under way stops when sync is turned off. After
// every pass, sync on or off, it runs a kept recordings pass (kept, nil for
// none), which erases the raw recordings the pass just synced and saves those
// still kept, and it retries the erase a deletion still owes on the unit
// (storage.RetryOwedErasure). As it starts and after every pass it brings the
// status warnings of owed Drive replacements up to date from the store. Each
// of these runs as work the stop cuts off and waits for (background.Run); the
// sweep ends once the stop refuses a pass.
func sweepDriveSync(ctx context.Context, background backgroundWork, store syncSweepStore, drive *driveSync, warnings *warningsRegistry, every time.Duration, kept *keptRecordings) {
	refreshWarnings := func() {
		if err := refreshDriveReplacementWarnings(store, warnings, time.Now().UTC()); err != nil {
			log.Printf("gdrive replacement warnings: %v", err)
		}
	}
	first := true
	runPasses(ctx, background, every, func(ctx context.Context) {
		if !first {
			syncSweep(ctx, store, drive)
			kept.run(ctx)
			if err := store.RetryOwedErasure(); err != nil {
				slog.Warn("a deleted segment's erase from the database log or backup is still owed", "error", err)
			}
		}
		first = false
		refreshWarnings()
	})
}

// syncSweep is one pass of sweepDriveSync.
func syncSweep(ctx context.Context, store syncSweepStore, drive *driveSync) {
	if drive.Get() == nil {
		return
	}
	ids, err := store.GetSessionsNeedingSync()
	if err != nil {
		log.Printf("gdrive sweep error: %v", err)
		return
	}

	retryScheduled, err := store.GetSessionsBySyncState(storage.SyncStateRetryScheduled)
	if err != nil {
		log.Printf("gdrive retry sweep error: %v", err)
		return
	}

	now := time.Now().UTC()
	seen := make(map[string]struct{}, len(ids)+len(retryScheduled))
	for _, id := range ids {
		seen[id] = struct{}{}
	}

	for i := range retryScheduled {
		sess := &retryScheduled[i]
		if !gdrive.IsRetryReady(now, sess.RetryCount, sess.LastSyncAttempt) {
			continue
		}
		if _, ok := seen[sess.ID]; ok {
			continue
		}
		ids = append(ids, sess.ID)
		seen[sess.ID] = struct{}{}
	}

	for _, id := range ids {
		syncer := drive.Get()
		if syncer == nil {
			break
		}
		if err := syncer.SyncSession(ctx, id); err != nil {
			log.Printf("gdrive sweep: session %s: %v", id, err)
		}
	}
}
