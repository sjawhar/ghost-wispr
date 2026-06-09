package gdrive

import (
	"context"
	"fmt"
	"log/slog"
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
}

type uploader interface {
	Upload(ctx context.Context, folderName string, files []SyncFile) (string, error)
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

	syncSess := SyncSession{
		ID:                  sess.ID,
		Title:               sess.Title,
		StartedAt:           sess.StartedAt,
		EndedAt:             sess.EndedAt,
		Summary:             sess.Summary,
		SummaryPreset:       sess.SummaryPreset,
		CanonicalTranscript: sess.CanonicalTranscript,
	}

	now := time.Now().UTC()
	if err := o.updateSyncState(&sess, storage.SyncStateSyncing, sess.GDriveFolderID, sess.RetryCount, &now, ""); err != nil {
		return fmt.Errorf("update sync state syncing %s: %w", sessionID, err)
	}

	files, folderName, err := BuildSyncFiles(&syncSess, segments, sess.AudioPath)
	if err != nil {
		_ = o.updateSyncState(&sess, storage.SyncStateFailed, sess.GDriveFolderID, sess.RetryCount, nil, err.Error())
		return fmt.Errorf("build sync files %s: %w", sessionID, err)
	}

	driveFolderID, err := o.uploader.Upload(ctx, folderName, files)
	if err != nil {
		attemptedAt := time.Now().UTC()
		errMsg := err.Error()
		if IsRemoteDeletedError(err) {
			_ = o.updateSyncState(&sess, storage.SyncStateRemoteDeleted, sess.GDriveFolderID, sess.RetryCount, &attemptedAt, errMsg)
			o.logger.Warn("gdrive remote deletion soft-deleted session", "operation", "sync_session", "session_id", sessionID, "sync_state", storage.SyncStateRemoteDeleted, "error", errMsg)
			return fmt.Errorf("remote folder deleted for %s: %w", sessionID, err)
		}

		_ = o.updateSyncState(&sess, storage.SyncStateFailed, sess.GDriveFolderID, sess.RetryCount, &attemptedAt, errMsg)
		plan := BuildRetryPlan(attemptedAt, sess.RetryCount)
		if !plan.Exhausted {
			_ = o.updateSyncState(&sess, storage.SyncStateRetryScheduled, sess.GDriveFolderID, plan.RetryCount, &attemptedAt, errMsg)
			o.logger.Warn("gdrive sync scheduled retry", "operation", "sync_session", "session_id", sessionID, "sync_state", storage.SyncStateRetryScheduled, "retry_count", plan.RetryCount, "next_attempt_at", plan.NextAttemptAt.Format(time.RFC3339), "error", errMsg)
			return fmt.Errorf("upload %s (retry scheduled): %w", sessionID, err)
		}

		o.logger.Error("gdrive sync permanently failed", "operation", "sync_session", "session_id", sessionID, "sync_state", storage.SyncStateFailed, "retry_count", sess.RetryCount, "error", errMsg)
		return fmt.Errorf("upload %s: %w", sessionID, err)
	}

	if err := o.updateSyncState(&sess, storage.SyncStateSynced, driveFolderID, 0, nil, ""); err != nil {
		return fmt.Errorf("update sync status %s: %w", sessionID, err)
	}

	o.logger.Info("gdrive session synced", "operation", "sync_session", "session_id", sessionID, "drive_folder_id", driveFolderID, "sync_state", storage.SyncStateSynced)
	return nil
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
