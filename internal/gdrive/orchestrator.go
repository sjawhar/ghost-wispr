package gdrive

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/logging"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

type OrchestratorStore interface {
	GetSession(id string) (storage.Session, error)
	GetSegments(sessionID string) ([]transcribe.Segment, error)
	UpdateSyncStatus(sessionID, status, driveFolderID string) error
	UpdateSyncState(sessionID, state, driveFolderID string, retryCount int, lastSyncAttempt *time.Time, errorMessage string) error
	// MarkSynced marks the session synced only while unchanged reports true
	// for it and its segments as they are now, and reports whether it did.
	MarkSynced(sessionID, driveFolderID string, unchanged func(storage.Session, []transcribe.Segment) bool) (bool, error)
}

type uploader interface {
	Upload(ctx context.Context, folderName string, files []SyncFile) (string, error)
	UploadInto(ctx context.Context, folderID string, files []SyncFile) error
}

type Orchestrator struct {
	uploader uploader
	store    OrchestratorStore
	logger   *slog.Logger
}

func NewOrchestrator(syncer *Syncer, store OrchestratorStore, logger ...*slog.Logger) *Orchestrator {
	l := logging.WithModule(slog.Default(), "gdrive")
	if len(logger) > 0 && logger[0] != nil {
		l = logging.WithModule(logger[0], "gdrive")
	}

	return &Orchestrator{uploader: syncer, store: store, logger: l}
}

func NewOrchestratorWithUploader(u uploader, store OrchestratorStore, logger ...*slog.Logger) *Orchestrator {
	l := logging.WithModule(slog.Default(), "gdrive")
	if len(logger) > 0 && logger[0] != nil {
		l = logging.WithModule(logger[0], "gdrive")
	}

	return &Orchestrator{uploader: u, store: store, logger: l}
}

// CanSync reports why a sync of the session cannot start, if it cannot: the
// session is missing, already synced, or in a state no sync starts from (its
// Drive folder was deleted by hand). A manual retry checks it first, so the
// API can say so instead of starting a sync that does nothing.
func (o *Orchestrator) CanSync(sessionID string) error {
	sess, err := o.store.GetSession(sessionID)
	if err != nil {
		return fmt.Errorf("get session %s: %w", sessionID, err)
	}
	state := sess.SyncState
	if state == "" {
		state = storage.SyncStatePending
	}
	if state == storage.SyncStateSynced {
		return fmt.Errorf("session %s is already synced", sessionID)
	}
	if err := ValidateSyncStateTransition(SyncState(state), SyncStateSyncing); err != nil {
		return fmt.Errorf("cannot sync session %s now: %w", sessionID, err)
	}
	return nil
}

func (o *Orchestrator) SyncSession(ctx context.Context, sessionID string) error {
	sess, err := o.store.GetSession(sessionID)
	if err != nil {
		return fmt.Errorf("get session %s: %w", sessionID, err)
	}

	if sess.SyncState == storage.SyncStateSynced {
		o.logger.Info("gdrive session already synced, skipping", "operation", "sync_session", "session_id", sessionID, "sync_state", storage.SyncStateSynced)
		return nil
	}

	segments, err := o.store.GetSegments(sessionID)
	if err != nil {
		return fmt.Errorf("get segments %s: %w", sessionID, err)
	}

	syncSess := toSyncSession(&sess)

	now := time.Now().UTC()
	if err := o.updateSyncState(&sess, storage.SyncStateSyncing, sess.GDriveFolderID, sess.RetryCount, &now, ""); err != nil {
		return fmt.Errorf("update sync state syncing %s: %w", sessionID, err)
	}

	files, folderName, err := BuildSyncFiles(&syncSess, segments, sess.AudioPath)
	if err != nil {
		_ = o.updateSyncState(&sess, storage.SyncStateFailed, sess.GDriveFolderID, sess.RetryCount, nil, err.Error())
		return fmt.Errorf("build sync files %s: %w", sessionID, err)
	}

	// A session synced before keeps its folder, and so does one whose first
	// sync created its folder and then failed: syncing it again adds what the
	// folder lacks instead of creating a second folder.
	driveFolderID := sess.GDriveFolderID
	if driveFolderID != "" {
		err = o.uploader.UploadInto(ctx, driveFolderID, files)
	} else {
		driveFolderID, err = o.uploader.Upload(ctx, folderName, files)
	}
	// A recording the upload left out (missing from disk when the file list
	// was built: its disk not mounted, say) was not uploaded: the session is
	// not synced until a retry uploads it. Marked synced, GC (gated on sync)
	// could remove that recording once it is back, though Drive never had it.
	// This is decided from the files built for the upload, not from a second
	// look at the disk, which a recording coming back meanwhile would pass.
	if missing := notUploaded(sess.AudioPath, files); err == nil && len(missing) > 0 {
		err = fmt.Errorf("recordings not on disk, not uploaded: %s", strings.Join(missing, ", "))
	}
	if err == nil {
		// Synced only if the session is still what was uploaded: GC may have
		// put back in it, during the upload, a recording it could not
		// remove, and its summary or transcript may have changed (made
		// again, refined), so a sync that read it earlier uploaded an older
		// one, perhaps over the current one, kept in Drive's history.
		summary, transcript := renderDocuments(&syncSess, segments)
		marked, markErr := o.store.MarkSynced(sessionID, driveFolderID, func(now storage.Session, nowSegments []transcribe.Segment) bool {
			nowSync := toSyncSession(&now)
			nowSummary, nowTranscript := renderDocuments(&nowSync, nowSegments)
			return now.AudioPath == sess.AudioPath && nowSummary == summary && nowTranscript == transcript
		})
		if markErr != nil {
			return fmt.Errorf("update sync status %s: %w", sessionID, markErr)
		}
		if marked {
			o.logger.Info("gdrive session synced", "operation", "sync_session", "session_id", sessionID, "drive_folder_id", driveFolderID, "sync_state", storage.SyncStateSynced)
			return nil
		}
		err = errors.New("the session changed during the sync (its audio, summary or transcript); the retry uploads it as it is now")
	}
	if err != nil {
		attemptedAt := time.Now().UTC()
		errMsg := err.Error()
		if IsRemoteDeletedError(err) {
			_ = o.updateSyncState(&sess, storage.SyncStateRemoteDeleted, driveFolderID, sess.RetryCount, &attemptedAt, errMsg)
			o.logger.Warn("gdrive remote deletion soft-deleted session", "operation", "sync_session", "session_id", sessionID, "sync_state", storage.SyncStateRemoteDeleted, "error", errMsg)
			return fmt.Errorf("remote folder deleted for %s: %w", sessionID, err)
		}

		_ = o.updateSyncState(&sess, storage.SyncStateFailed, driveFolderID, sess.RetryCount, &attemptedAt, errMsg)
		plan := BuildRetryPlan(attemptedAt, sess.RetryCount)
		if !plan.Exhausted {
			_ = o.updateSyncState(&sess, storage.SyncStateRetryScheduled, driveFolderID, plan.RetryCount, &attemptedAt, errMsg)
			o.logger.Warn("gdrive sync scheduled retry", "operation", "sync_session", "session_id", sessionID, "sync_state", storage.SyncStateRetryScheduled, "retry_count", plan.RetryCount, "next_attempt_at", plan.NextAttemptAt.Format(time.RFC3339), "error", errMsg)
			return fmt.Errorf("upload %s (retry scheduled): %w", sessionID, err)
		}

		o.logger.Error("gdrive sync permanently failed", "operation", "sync_session", "session_id", sessionID, "sync_state", storage.SyncStateFailed, "retry_count", sess.RetryCount, "error", errMsg)
		return fmt.Errorf("upload %s: %w", sessionID, err)
	}
	return nil
}

// toSyncSession is what a sync renders of sess.
func toSyncSession(sess *storage.Session) SyncSession {
	return SyncSession{
		ID:                  sess.ID,
		Title:               sess.Title,
		StartedAt:           sess.StartedAt,
		EndedAt:             sess.EndedAt,
		Summary:             sess.Summary,
		SummaryPreset:       sess.SummaryPreset,
		CanonicalTranscript: sess.CanonicalTranscript,
	}
}

func (o *Orchestrator) updateSyncState(sess *storage.Session, nextState, driveFolderID string, retryCount int, lastSyncAttempt *time.Time, errorMessage string) error {
	current := sess.SyncState
	if current == "" {
		current = storage.SyncStatePending
	}

	if err := ValidateSyncStateTransition(SyncState(current), SyncState(nextState)); err != nil {
		return err
	}

	if err := o.store.UpdateSyncState(sess.ID, nextState, driveFolderID, retryCount, lastSyncAttempt, errorMessage); err != nil {
		return err
	}

	sess.SyncState = nextState
	sess.RetryCount = retryCount
	sess.ErrorMessage = errorMessage
	sess.GDriveFolderID = driveFolderID
	sess.LastSyncAttempt = lastSyncAttempt
	return nil
}
