package gdrive

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/ghost-wispr/internal/logging"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// OrchestratorStore is what a sync reads and writes in the store. A sync
// holds its session from ClaimSync until MarkSynced or EndSync, each under
// the claim it made; a write under a claim the session no longer has changes
// nothing and reports false.
type OrchestratorStore interface {
	GetSession(id string) (storage.Session, error)
	GetSegments(sessionID string) ([]transcribe.Segment, error)
	// ClaimSync moves the session to SYNCING for the sync claim names, only
	// while its sync state is still from and no sync holds it, and returns
	// it as claimed.
	ClaimSync(sessionID, from, claim string, at time.Time) (storage.Session, bool, error)
	// MarkSynced marks the session synced only while the sync claim names
	// holds it and unchanged reports true for it and its segments as they
	// are now, and reports whether it did.
	MarkSynced(sessionID, claim, driveFolderID string, unchanged func(storage.Session, []transcribe.Segment) bool) (bool, error)
	// EndSync records how the sync claim names ended, when it did not mark
	// the session synced, only while that sync holds it, and reports whether
	// it did.
	EndSync(sessionID, claim, state, driveFolderID string, retryCount int, at time.Time, errorMessage string) (bool, error)
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
// session is missing, already synced, being synced now, or in a state no sync
// starts from (its Drive folder was deleted by hand). A manual retry checks it
// first, so the API can say so instead of starting a sync that does nothing.
func (o *Orchestrator) CanSync(sessionID string) error {
	sess, err := o.store.GetSession(sessionID)
	if err != nil {
		return fmt.Errorf("get session %s: %w", sessionID, err)
	}
	switch state := syncStateOf(&sess); state {
	case storage.SyncStateSynced:
		return fmt.Errorf("session %s is already synced", sessionID)
	case storage.SyncStateSyncing:
		return fmt.Errorf("session %s is being synced now", sessionID)
	default:
		if err := ValidateSyncStateTransition(SyncState(state), SyncStateSyncing); err != nil {
			return fmt.Errorf("cannot sync session %s now: %w", sessionID, err)
		}
	}
	return nil
}

// syncStateOf is sess's sync state, PENDING when it has none.
func syncStateOf(sess *storage.Session) string {
	if sess.SyncState == "" {
		return storage.SyncStatePending
	}
	return sess.SyncState
}

// SyncSession uploads the session to Drive and marks it synced. It starts
// nothing for a session already synced, or one another sync holds: a sync
// claims its session first (ClaimSync), so retry-sync, the sync after a
// summary and the sweep never sync one session at once. Every state it
// writes after the claim is under that claim, so a sync that no longer holds
// its session changes nothing in it.
func (o *Orchestrator) SyncSession(ctx context.Context, sessionID string) error {
	read, err := o.store.GetSession(sessionID)
	if err != nil {
		return fmt.Errorf("get session %s: %w", sessionID, err)
	}
	switch state := syncStateOf(&read); state {
	case storage.SyncStateSynced:
		o.logger.Info("gdrive session already synced, skipping", "operation", "sync_session", "session_id", sessionID, "sync_state", state)
		return nil
	case storage.SyncStateSyncing:
		o.logger.Info("gdrive session is being synced by another sync, skipping", "operation", "sync_session", "session_id", sessionID, "sync_state", state)
		return nil
	default:
		if err := ValidateSyncStateTransition(SyncState(state), SyncStateSyncing); err != nil {
			return fmt.Errorf("sync session %s: %w", sessionID, err)
		}
	}

	segments, err := o.store.GetSegments(sessionID)
	if err != nil {
		return fmt.Errorf("get segments %s: %w", sessionID, err)
	}

	claim := uuid.NewString()
	sess, claimed, err := o.store.ClaimSync(sessionID, read.SyncState, claim, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("claim the sync of %s: %w", sessionID, err)
	}
	if !claimed {
		o.logger.Info("gdrive session was claimed by another sync first, skipping", "operation", "sync_session", "session_id", sessionID)
		return nil
	}
	syncSess := toSyncSession(&sess)

	files, folderName, err := BuildSyncFiles(&syncSess, segments, sess.AudioPath)
	if err != nil {
		o.endSync(sessionID, claim, storage.SyncStateFailed, sess.GDriveFolderID, sess.RetryCount, time.Now().UTC(), err)
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
		// again, refined).
		summary, transcript := renderDocuments(&syncSess, segments)
		marked, markErr := o.store.MarkSynced(sessionID, claim, driveFolderID, func(now storage.Session, nowSegments []transcribe.Segment) bool {
			nowSync := toSyncSession(&now)
			nowSummary, nowTranscript := renderDocuments(&nowSync, nowSegments)
			return now.AudioPath == sess.AudioPath && nowSummary == summary && nowTranscript == transcript
		})
		switch {
		case markErr != nil:
			// Left SYNCING, the session would never be synced again: the
			// sweep syncs none, and every other sync finds it claimed.
			err = fmt.Errorf("mark synced: %w", markErr)
		case marked:
			o.logger.Info("gdrive session synced", "operation", "sync_session", "session_id", sessionID, "drive_folder_id", driveFolderID, "sync_state", storage.SyncStateSynced)
			return nil
		default:
			// Not a failure: the session is back to pending sync, as a
			// change after a sync sends it, with its folder and retry
			// count, and the next sync uploads it as it is now.
			changed := errors.New("the session changed during the sync (its audio, summary or transcript); it is back to pending sync, and the next sync uploads it as it is now")
			if !o.endSync(sessionID, claim, storage.SyncStatePending, driveFolderID, sess.RetryCount, time.Now().UTC(), changed) {
				return fmt.Errorf("sync %s: the session changed during the sync and was left as it is: this sync no longer holds it, or its end could not be written", sessionID)
			}
			o.logger.Info("gdrive session changed during the sync, back to pending", "operation", "sync_session", "session_id", sessionID, "drive_folder_id", driveFolderID, "sync_state", storage.SyncStatePending)
			return fmt.Errorf("sync %s: %w", sessionID, changed)
		}
	}

	attemptedAt := time.Now().UTC()
	if IsRemoteDeletedError(err) {
		if o.endSync(sessionID, claim, storage.SyncStateRemoteDeleted, driveFolderID, sess.RetryCount, attemptedAt, err) {
			o.logger.Warn("gdrive remote deletion soft-deleted session", "operation", "sync_session", "session_id", sessionID, "sync_state", storage.SyncStateRemoteDeleted, "error", err.Error())
		}
		return fmt.Errorf("remote folder deleted for %s: %w", sessionID, err)
	}
	plan := BuildRetryPlan(attemptedAt, sess.RetryCount)
	if !o.endSync(sessionID, claim, string(plan.NextState), driveFolderID, plan.RetryCount, attemptedAt, err) {
		return fmt.Errorf("upload %s: %w", sessionID, err)
	}
	if !plan.Exhausted {
		o.logger.Warn("gdrive sync scheduled retry", "operation", "sync_session", "session_id", sessionID, "sync_state", storage.SyncStateRetryScheduled, "retry_count", plan.RetryCount, "next_attempt_at", plan.NextAttemptAt.Format(time.RFC3339), "error", err.Error())
		return fmt.Errorf("upload %s (retry scheduled): %w", sessionID, err)
	}
	o.logger.Error("gdrive sync permanently failed", "operation", "sync_session", "session_id", sessionID, "sync_state", storage.SyncStateFailed, "retry_count", sess.RetryCount, "error", err.Error())
	return fmt.Errorf("upload %s: %w", sessionID, err)
}

// endSync ends the sync of session sessionID under claim, which did not mark
// it synced, moving it to state with retryCount, the Drive folder the sync
// used, the time it was tried and why it failed (syncErr). It reports whether
// it did. A sync that no longer holds its session leaves it as it is, and a
// session whose end could not be written stays SYNCING until the app
// restarts (storage.RecoverInterruptedSyncs).
func (o *Orchestrator) endSync(sessionID, claim, state, driveFolderID string, retryCount int, at time.Time, syncErr error) bool {
	ended, err := o.store.EndSync(sessionID, claim, state, driveFolderID, retryCount, at, syncErr.Error())
	switch {
	case err != nil:
		o.logger.Error("gdrive sync could not record its end; the session stays mid-sync until the app restarts", "operation", "sync_session", "session_id", sessionID, "sync_state", state, "error", err, "sync_error", syncErr.Error())
	case !ended:
		o.logger.Warn("gdrive sync no longer holds its session; its state is left as it is", "operation", "sync_session", "session_id", sessionID, "sync_error", syncErr.Error())
	}
	return ended
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
