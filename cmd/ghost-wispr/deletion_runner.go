package main

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/api/drive/v3"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// segmentDeleteRunner runs "Delete text" and "Delete text and audio"
// (deletion_actions.go), checks each cut (deletion_cut_check.go), restores
// the recordings a deletion or a refinement needs (deletion_restore.go) and
// finishes, as the app starts, a deletion a stop cut off
// (deletion_recovery.go). The app makes one when it starts
// (newAppDeleteRunner) and keeps it: its locks hold one deletion, restore or
// recovery of a session at a time. Every dependency is required
// (newSegmentDeleteRunner).
type segmentDeleteRunner struct {
	store *storage.SQLiteStore
	// audioDir returns the audio directory as the configuration has it now.
	audioDir func() string
	// ensureRecordings finds a session's recordings, restoring from Drive
	// the ones this unit lacks (ensureDeletionRecordings).
	ensureRecordings func(context.Context, storage.Session) (deletionRecordingSet, error)
	duration         func(context.Context, string) (float64, error)
	splice           func(context.Context, string, float64, float64) (string, error)
	refine           func(context.Context, string, string, time.Time) error
	summarize        func(context.Context, string, time.Time) error
	// transcribeCut transcribes a cut copy of a recording with the model the
	// session was refined with, for the check after each cut of "Delete text
	// and audio" (cutAndCheck).
	transcribeCut func(ctx context.Context, model, path string) ([]transcribe.TimestampedWord, error)
	// refused keeps a refused cut's /api/status warning while its segment is
	// there.
	refused *refusedCuts

	locks sessionDeletionLocks
}

// newSegmentDeleteRunner returns r once it has every dependency. A missing
// one is an error here, never a step skipped later: without transcribeCut a
// cut would go unchecked against the consent rule.
func newSegmentDeleteRunner(r *segmentDeleteRunner) (*segmentDeleteRunner, error) {
	var missing []string
	for name, absent := range map[string]bool{
		"store":            r.store == nil,
		"audioDir":         r.audioDir == nil,
		"ensureRecordings": r.ensureRecordings == nil,
		"duration":         r.duration == nil,
		"splice":           r.splice == nil,
		"refine":           r.refine == nil,
		"summarize":        r.summarize == nil,
		"transcribeCut":    r.transcribeCut == nil,
		"refused":          r.refused == nil,
	} {
		if absent {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("segment deletion is missing %s", strings.Join(missing, ", "))
	}
	return r, nil
}

// newAppDeleteRunner makes the app's runner. Each action reads the
// configuration as it is then: its audio directory, its Drive folder and the
// batch transcription that checks a cut. Batch transcription has already
// persisted the exact word spans, so deleting never creates a locator.
func newAppDeleteRunner(store *storage.SQLiteStore, cfgStore *config.Store, refine func(context.Context, string, string, time.Time) error, summarize func(context.Context, string, time.Time) error, refused *refusedCuts, logger *slog.Logger) (*segmentDeleteRunner, error) {
	return newSegmentDeleteRunner(&segmentDeleteRunner{
		store:    store,
		audioDir: func() string { return cfgStore.Get().AudioDir },
		ensureRecordings: func(ctx context.Context, session storage.Session) (deletionRecordingSet, error) {
			cfg := cfgStore.Get()
			return ensureDeletionRecordings(ctx, store, &session, cfg.AudioDir, func(ctx context.Context) (deletionRecordingSource, error) {
				if cfg.GDriveFolderID == "" {
					return nil, fmt.Errorf("google Drive root folder is not configured")
				}
				syncer, err := newDriveSyncer(ctx, &cfg, logger)
				if err != nil {
					return nil, err
				}
				return syncer, nil
			})
		},
		duration:  audio.Duration,
		splice:    audio.Splice,
		refine:    refine,
		summarize: summarize,
		transcribeCut: func(ctx context.Context, model, path string) ([]transcribe.TimestampedWord, error) {
			cfg := cfgStore.Get()
			transcriber, err := makeBatchTranscriber(&cfg)
			if err != nil {
				return nil, fmt.Errorf("batch transcription, which checks the cut, is unavailable: %w", err)
			}
			if selector, ok := transcriber.(transcribe.ModelSelector); ok && model != "" {
				transcriber = selector.WithModel(model)
			}
			result, err := transcriber.Transcribe(ctx, path)
			if err != nil {
				return nil, err
			}
			return result.Words, nil
		},
		refused: refused,
	})
}

type deletionRecordingSet struct {
	Paths      []string
	Downloaded []string
}

type deletionRecordingSource interface {
	ListFolderFiles(ctx context.Context, folderID string) ([]*drive.File, error)
	DownloadRecording(ctx context.Context, folderID, name, destination string) error
}

type recordingRestoreGuard interface {
	LatestRecordingSHA256(sessionID, recording string) (string, bool, error)
	RefinementRecordingSources(sessionID string) ([]storage.RefinementRecordingSource, error)
}

// sessionDeletionLocks holds one deletion, recording restore or recovery of a
// session at a time, and marks the sessions a recovery is restoring.
type sessionDeletionLocks struct {
	mu         sync.Mutex
	locks      map[string]*sync.Mutex
	recovering map[string]bool
}

func (l *sessionDeletionLocks) lock(sessionID string) func() {
	l.mu.Lock()
	if l.locks == nil {
		l.locks = make(map[string]*sync.Mutex)
	}
	lock := l.locks[sessionID]
	if lock == nil {
		lock = &sync.Mutex{}
		l.locks[sessionID] = lock
	}
	l.mu.Unlock()
	lock.Lock()
	return lock.Unlock
}

func (l *sessionDeletionLocks) setRecovering(sessionID string, recovering bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.recovering == nil {
		l.recovering = make(map[string]bool)
	}
	if recovering {
		l.recovering[sessionID] = true
	} else {
		delete(l.recovering, sessionID)
	}
}

func (l *sessionDeletionLocks) isRecovering(sessionID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.recovering[sessionID]
}

// lockForRecovery marks session sessionID as recovering, so a deletion of it
// is refused (recordingRestoreInProgress) instead of waiting behind the
// recovery, and takes its lock. unlock releases the lock early, once; done
// releases it if unlock has not, then ends the mark.
func (l *sessionDeletionLocks) lockForRecovery(sessionID string) (unlock, done func()) {
	l.setRecovering(sessionID, true)
	release := l.lock(sessionID)
	var once sync.Once
	unlock = func() { once.Do(release) }
	return unlock, func() {
		unlock()
		l.setRecovering(sessionID, false)
	}
}

type recordingRestoreInProgressError struct {
	cause error
}

func (e recordingRestoreInProgressError) Error() string { return e.cause.Error() }

func (e recordingRestoreInProgressError) Unwrap() error { return e.cause }

func (recordingRestoreInProgressError) RecordingRestoreInProgress() {}

func recordingRestoreInProgress() error {
	return recordingRestoreInProgressError{cause: storage.ErrSegmentSplicePending}
}
