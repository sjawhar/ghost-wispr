package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/gdrive"
	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// sessionSyncer syncs one session to Google Drive.
type sessionSyncer interface {
	// CanSync reports, at once, why a sync of the session cannot start, if
	// it cannot.
	CanSync(ctx context.Context, sessionID string) error
	SyncSession(ctx context.Context, sessionID string) error
	// FolderOnDrive reports whether Drive has a session folder out of the
	// trash; GC asks it before it deletes a session or removes its audio.
	FolderOnDrive(ctx context.Context, folderID string) (bool, error)
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

// retrySync starts a sync of session sessionID in the background with
// syncer, the running Drive syncer (nil while sync is off), and says at once
// if none can start (for a session whose Drive folder was deleted, while
// Drive says it is not back), so the API does not report a retry that does
// nothing. Drive not answering whether such a folder is back is
// server.ErrDriveUnavailable: the retry may work once Drive answers.
func retrySync(ctx context.Context, syncer sessionSyncer, sessionID string) error {
	if syncer == nil {
		return server.ErrSyncNotConfigured
	}
	if err := syncer.CanSync(ctx, sessionID); err != nil {
		if errors.Is(err, gdrive.ErrDriveDidNotAnswer) {
			return fmt.Errorf("%w: %w", server.ErrDriveUnavailable, err)
		}
		return err
	}
	go func() {
		if err := syncer.SyncSession(context.Background(), sessionID); err != nil {
			log.Printf("gdrive retry: session %s: %v", sessionID, err)
		}
	}()
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
				log.Printf("config: gdrive sync not started: %v", err)
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

// syncSweepStore lists the sessions the sweep syncs.
type syncSweepStore interface {
	GetSessionsNeedingSync() ([]string, error)
	GetSessionsBySyncState(syncState string) ([]storage.Session, error)
}

// sweepDriveSync syncs, every interval until ctx ends, the sessions waiting
// for their first sync and those whose retry is due. While sync is off it
// syncs nothing, and a sweep under way stops when sync is turned off.
func sweepDriveSync(ctx context.Context, store syncSweepStore, drive *driveSync, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if drive.Get() == nil {
				continue
			}
			ids, err := store.GetSessionsNeedingSync()
			if err != nil {
				log.Printf("gdrive sweep error: %v", err)
				continue
			}

			retryScheduled, err := store.GetSessionsBySyncState(storage.SyncStateRetryScheduled)
			if err != nil {
				log.Printf("gdrive retry sweep error: %v", err)
				continue
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
	}
}
