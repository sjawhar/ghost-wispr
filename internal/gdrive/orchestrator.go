package gdrive

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"google.golang.org/api/drive/v3"

	"github.com/sjawhar/ghost-wispr/internal/logging"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// OrchestratorStore is what the orchestrator reads and writes in the store:
// the sync of a session (syncStore), the replacement of its Drive files that
// a segment deletion owes (replacementStore), and the documents of merged
// sources that "Delete text" changed (documentsStore).
type OrchestratorStore interface {
	syncStore
	replacementStore
	documentsStore
}

// syncStore is what a sync reads and writes. A sync holds its session from
// ClaimSync until MarkSynced or EndSync, each under the claim it made; a
// write under a claim the session no longer has changes nothing and reports
// false.
type syncStore interface {
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
	// RecordSyncFolder stores a newly created Drive folder while the sync
	// claim still holds the session, before its first upload.
	RecordSyncFolder(sessionID, claim, driveFolderID string) (bool, error)
	// EndSync records how the sync claim names ended, when it did not mark
	// the session synced, only while that sync holds it, and reports whether
	// it did.
	EndSync(sessionID, claim, state, driveFolderID string, retryCount int, at time.Time, errorMessage string) (bool, error)
}

// replacementStore is the ledger of the Drive replacements segment deletions
// owe: which deletion generation, which folders, and each replaced file.
type replacementStore interface {
	GetSession(id string) (storage.Session, error)
	DriveReplacementInput(sessionID string) (pending bool, splicedRecording string, err error)
	DeletionGeneration(sessionID string) (int, error)
	HasPendingSplice(sessionID string) (bool, error)
	OldestPendingSplice(sessionID string) (time.Time, bool, error)
	// EnsureDriveReplacementFolders reads the deletion's owed folders,
	// rebuilding a ledger that is empty while the deletion is pending.
	EnsureDriveReplacementFolders(sessionID string, generation int) ([]storage.DriveReplacementFolder, error)
	ClearDriveReplacementPending(sessionID string, generation int) error
	PendingDriveReplacementRecordings(sessionID string) ([]string, error)
	LatestRecordingSHA256(sessionID, recording string) (string, bool, error)
	DriveReplacements(sessionID string, generation int) ([]storage.DriveReplacement, error)
	StartDriveReplacement(sessionID, name string, generation int, oldIDs []string) error
	MarkDriveReplacementUploaded(sessionID, name string, generation int, newID string) error
	MarkDriveReplacementDeleted(sessionID, name, oldID string, generation int) error
	MarkDriveReplacementFolder(sessionID string, generation int, folderSessionID, state string) error
	FinishDriveReplacement(sessionID string, generation int) error
	// RecordDriveReplacementFailure counts a failed replacement attempt of
	// the session's deletion generation, with why; the app's status warning
	// is read from these counts (storage.OwedDriveReplacements).
	RecordDriveReplacementFailure(sessionID string, generation int, message string) error
}

// driveClient is the Google Drive the orchestrator works on: *Syncer, or a
// test double.
type driveClient interface {
	syncDrive
	replacementDrive
}

// syncDrive is what a sync, a documents refresh and GC ask of Drive.
type syncDrive interface {
	// UploadWithFolderRecorded creates a session folder, calls record with
	// its id before the first file upload, then uploads files into it.
	UploadWithFolderRecorded(ctx context.Context, folderName string, files []SyncFile, record func(folderID string) error) (string, error)
	UploadInto(ctx context.Context, folderID string, files []SyncFile) error
	RenameFolder(ctx context.Context, folderID, name string) error
	FolderOnDrive(ctx context.Context, folderID string) (bool, error)
	RecordingsMatch(ctx context.Context, folderID string, checksums map[string]string) (bool, error)
}

// replacementDrive is the Drive file operations a replacement runs, one
// replacement at a time and no upload meanwhile (Serialize).
type replacementDrive interface {
	// Serialize runs fn while no other upload or replacement through this
	// Drive runs.
	Serialize(fn func() error) error
	// CheckFolder returns nil while Drive has the folder out of the trash
	// (Syncer.checkFolder).
	CheckFolder(ctx context.Context, folderID string) error
	ListFolderFiles(ctx context.Context, folderID string) ([]*drive.File, error)
	// UploadFile uploads one file into a folder and returns its id.
	UploadFile(ctx context.Context, folderID string, file *SyncFile) (string, error)
	// VerifyFile checks that Drive holds file fileID with expected's
	// content.
	VerifyFile(ctx context.Context, fileID string, expected *SyncFile) error
	// DeleteFile deletes a file; one already gone is no error.
	DeleteFile(ctx context.Context, fileID string) error
	RenameFolder(ctx context.Context, folderID, name string) error
}

type Orchestrator struct {
	uploader driveClient
	store    OrchestratorStore
	logger   *slog.Logger
	// audioDir is the audio directory a recording is uploaded from when it
	// is not where its entry points (recording.Resolve).
	audioDir string
}

// SetAudioDir sets the audio directory a sync reads a recording from when it
// is not where the session's entry points (recording.Resolve). Call it before
// the orchestrator syncs.
func (o *Orchestrator) SetAudioDir(dir string) {
	o.audioDir = dir
}

// replacementFailed records a failed replacement attempt of sessionID's
// deletion generation, so a replacement that keeps failing is reported, also
// after a restart.
func (o *Orchestrator) replacementFailed(sessionID string, generation int, cause error) {
	if err := o.store.RecordDriveReplacementFailure(sessionID, generation, cause.Error()); err != nil {
		o.logger.Error("gdrive replacement could not record its failure", "operation", "replace_session_files", "session_id", sessionID, "error", err, "replacement_error", cause.Error())
	}
}

func NewOrchestrator(syncer *Syncer, store OrchestratorStore, logger ...*slog.Logger) *Orchestrator {
	return NewOrchestratorWithUploader(syncer, store, logger...)
}

func NewOrchestratorWithUploader(u driveClient, store OrchestratorStore, logger ...*slog.Logger) *Orchestrator {
	l := logging.WithModule(slog.Default(), "gdrive")
	if len(logger) > 0 && logger[0] != nil {
		l = logging.WithModule(logger[0], "gdrive")
	}

	return &Orchestrator{uploader: u, store: store, logger: l}
}

func (o *Orchestrator) recordSyncFolder(sessionID, claim, folderID string) error {
	recorded, err := o.store.RecordSyncFolder(sessionID, claim, folderID)
	if err != nil {
		return fmt.Errorf("record Drive folder %s: %w", folderID, err)
	}
	if !recorded {
		return fmt.Errorf("record Drive folder %s: the sync no longer holds session %s", folderID, sessionID)
	}
	return nil
}

// FolderOnDrive reports whether Drive has session folder folderID out of the
// trash, and an error when Drive cannot say. GC asks it before it deletes a
// synced session or removes its audio.
func (o *Orchestrator) FolderOnDrive(ctx context.Context, folderID string) (bool, error) {
	return o.uploader.FolderOnDrive(ctx, folderID)
}

// RecordingsMatch reports whether Drive holds all supplied recordings with
// their matching MD5 checksums. GC asks it before removing local audio.
func (o *Orchestrator) RecordingsMatch(ctx context.Context, folderID string, checksums map[string]string) (bool, error) {
	return o.uploader.RecordingsMatch(ctx, folderID, checksums)
}

// CanSync reports why a sync of the session cannot start, if it cannot: the
// session is missing, already synced, being synced now, or in a state no sync
// starts from. For a session whose Drive folder was deleted (REMOTE_DELETED)
// it asks Drive, as the sync would, and says so while the folder is still in
// the trash or gone, or Drive does not answer. A manual retry checks it
// first, so the API answers with why instead of starting a sync that does
// nothing.
func (o *Orchestrator) CanSync(ctx context.Context, sessionID string) error {
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
		if err := syncableFrom(&sess); err != nil {
			return err
		}
		return o.folderBack(ctx, &sess)
	}
}

// syncableFrom returns why no sync starts from sess's sync state, or nil. A
// REMOTE_DELETED session needs its folder: a sync into a new one would make
// again what the user deleted.
func syncableFrom(sess *storage.Session) error {
	state := syncStateOf(sess)
	if err := ValidateSyncStateTransition(SyncState(state), SyncStateSyncing); err != nil {
		return fmt.Errorf("cannot sync session %s now: %w", sess.ID, err)
	}
	if state == storage.SyncStateRemoteDeleted && sess.GDriveFolderID == "" {
		return fmt.Errorf("cannot sync session %s now: its Drive folder was deleted and no folder is recorded to look for", sess.ID)
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

func replacementReady(sess *storage.Session) bool {
	refinementDone := sess.RefinementStatus == storage.RefinementCompleted ||
		sess.RefinementStatus == storage.RefinementFailed ||
		sess.RefinementStatus == storage.RefinementNoAudio
	summaryDone := sess.SummaryStatus == storage.SummaryCompleted || sess.SummaryStatus == storage.SummaryFailed
	return refinementDone && summaryDone
}

// SyncSession uploads the session to Drive and marks it synced. It starts
// nothing for a session already synced, or one another sync holds: a sync
// claims its session first (ClaimSync), so retry-sync, the sync after a
// summary and the sweep never sync one session at once. Every state it
// writes after the claim is under that claim, so a sync that no longer holds
// its session changes nothing in it. First it updates in place the Drive
// documents of the merged sources "Delete text" in the session changed, or of
// the session itself when it is such a source (refreshOwedDocuments).
func (o *Orchestrator) SyncSession(ctx context.Context, sessionID string) error {
	refreshErr := o.refreshOwedDocuments(ctx, sessionID)
	err := o.syncSession(ctx, sessionID)
	if refreshErr == nil {
		return err
	}
	return errors.Join(refreshErr, err)
}

func (o *Orchestrator) syncSession(ctx context.Context, sessionID string) error {
	read, err := o.store.GetSession(sessionID)
	if err != nil {
		return fmt.Errorf("get session %s: %w", sessionID, err)
	}
	pending, splicedRecording, err := o.store.DriveReplacementInput(sessionID)
	if err != nil {
		return err
	}
	if pending {
		return o.replaceOwedFiles(ctx, sessionID, splicedRecording)
	}
	switch state := syncStateOf(&read); state {
	case storage.SyncStateSynced:
		o.logger.Info("gdrive session already synced, skipping", "operation", "sync_session", "session_id", sessionID, "sync_state", state)
		return nil
	case storage.SyncStateSyncing:
		o.logger.Info("gdrive session is being synced by another sync, skipping", "operation", "sync_session", "session_id", sessionID, "sync_state", state)
		return nil
	default:
		if err := syncableFrom(&read); err != nil {
			return err
		}
	}
	// A session whose Drive folder was deleted stays REMOTE_DELETED until
	// Drive shows the folder back: the sync holds it only then. Held first,
	// a Drive that did not answer would end the hold in a retry, and nothing
	// would then keep GC from the session while its folder is still in the
	// trash. (The app stopping while the sync holds it puts it back:
	// storage.RecoverInterruptedSyncs.)
	if err := o.folderBack(ctx, &read); err != nil {
		o.logger.Info("gdrive session stays as it is: its Drive folder is not back", "operation", "sync_session", "session_id", sessionID, "drive_folder_id", read.GDriveFolderID, "error", err)
		return err
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
	return o.syncClaimed(ctx, &sess, claim, segments)
}

// syncClaimed uploads sess, which the sync claim holds, with its segments,
// and marks it synced, or ends the sync as unsynced with why.
func (o *Orchestrator) syncClaimed(ctx context.Context, sess *storage.Session, claim string, segments []transcribe.Segment) error {
	sessionID := sess.ID
	syncSess := toSyncSession(sess)

	// Each recording is uploaded from where every reader of a recording
	// finds it (recording.Resolve); one found nowhere is left out, and
	// notUploaded names it.
	audioPath := resolvedAudioPath(sess.AudioPath, o.audioDir)
	files, folderName, unreadable, err := buildSyncFiles(&syncSess, segments, audioPath)
	if err != nil {
		o.endSync(sessionID, claim, storage.SyncStateFailed, sess.GDriveFolderID, sess.RetryCount, time.Now().UTC(), err)
		return fmt.Errorf("build sync files %s: %w", sessionID, err)
	}

	driveFolderID, err := o.uploadSession(ctx, sess, claim, files, folderName)
	// A recording the upload left out (missing from disk when the file list
	// was built: its disk not mounted, say; or there but unreadable) was not
	// uploaded: the session is not synced until a retry, with the sync's
	// backoff, uploads it. Marked synced, GC (gated on sync) could remove
	// that recording once it is back, though Drive never had it. This is
	// decided from the files built for the upload, not from a second look at
	// the disk, which a recording coming back meanwhile would pass.
	if err == nil && len(unreadable) != 0 {
		err = fmt.Errorf("recordings could not be read, not uploaded: %s", strings.Join(unreadable, ", "))
	}
	if missing := notUploaded(audioPath, files); err == nil && len(missing) > 0 {
		err = fmt.Errorf("recordings not on disk, not uploaded: %s", strings.Join(missing, ", "))
	}
	if err == nil {
		var done bool
		if done, err = o.markSynced(sess, claim, driveFolderID, &syncSess, segments); done {
			return err
		}
	}
	return o.endFailedSync(sess, claim, driveFolderID, err)
}

// uploadSession uploads files into sess's Drive folder, and returns the
// folder's id. A session synced before keeps its folder, and so does one
// whose first sync created its folder and then failed: syncing it again adds
// what the folder lacks instead of creating a second folder. A summary the
// session no longer has is emptied in that existing folder.
func (o *Orchestrator) uploadSession(ctx context.Context, sess *storage.Session, claim string, files []SyncFile, folderName string) (string, error) {
	driveFolderID := sess.GDriveFolderID
	if driveFolderID == "" {
		return o.uploader.UploadWithFolderRecorded(ctx, folderName, files, func(folderID string) error {
			return o.recordSyncFolder(sess.ID, claim, folderID)
		})
	}
	if err := o.uploader.UploadInto(ctx, driveFolderID, withEmptiedSummary(files)); err != nil {
		return driveFolderID, err
	}
	// Delete text can change the title that names the existing folder.
	// Rename it in place: its id and the links to its files stay.
	return driveFolderID, o.uploader.RenameFolder(ctx, driveFolderID, folderName)
}

// markSynced marks sess synced in Drive folder driveFolderID, only if the
// session is still what was uploaded (syncSess and segments): GC may have
// put back in it, during the upload, a recording it could not remove, and
// its summary or transcript may have changed (made again, refined). done is
// false, with why, when the mark could not be written: the sync then ends as
// a failed one.
func (o *Orchestrator) markSynced(sess *storage.Session, claim, driveFolderID string, syncSess *SyncSession, segments []transcribe.Segment) (done bool, err error) {
	sessionID := sess.ID
	summary, transcript := renderDocuments(syncSess, segments)
	marked, markErr := o.store.MarkSynced(sessionID, claim, driveFolderID, func(now storage.Session, nowSegments []transcribe.Segment) bool {
		nowSync := toSyncSession(&now)
		nowSummary, nowTranscript := renderDocuments(&nowSync, nowSegments)
		return now.AudioPath == sess.AudioPath && nowSummary == summary && nowTranscript == transcript
	})
	switch {
	case markErr != nil:
		// Left SYNCING, the session would never be synced again: the
		// sweep syncs none, and every other sync finds it claimed.
		return false, fmt.Errorf("mark synced: %w", markErr)
	case marked:
		o.logger.Info("gdrive session synced", "operation", "sync_session", "session_id", sessionID, "drive_folder_id", driveFolderID, "sync_state", storage.SyncStateSynced)
		return true, nil
	default:
		// Not a failure: the session is back to pending sync, as a
		// change after a sync sends it, with its folder and retry
		// count, and the next sync uploads it as it is now.
		changed := errors.New("the session changed during the sync (its audio, summary or transcript); it is back to pending sync, and the next sync uploads it as it is now")
		if !o.endSync(sessionID, claim, storage.SyncStatePending, driveFolderID, sess.RetryCount, time.Now().UTC(), changed) {
			return true, fmt.Errorf("sync %s: the session changed during the sync and was left as it is: this sync no longer holds it, or its end could not be written", sessionID)
		}
		o.logger.Info("gdrive session changed during the sync, back to pending", "operation", "sync_session", "session_id", sessionID, "drive_folder_id", driveFolderID, "sync_state", storage.SyncStatePending)
		return true, fmt.Errorf("sync %s: %w", sessionID, changed)
	}
}

// endFailedSync ends the sync of sess that failed with err: REMOTE_DELETED
// when its own folder went, else a retry or, after the last one, FAILED.
func (o *Orchestrator) endFailedSync(sess *storage.Session, claim, driveFolderID string, err error) error {
	sessionID := sess.ID
	attemptedAt := time.Now().UTC()
	// A 404 while syncing into the session's own folder, or that folder in
	// the trash, means it (or a file in it) was deleted on Drive: no sync
	// uploads into it again, and no GC path deletes the session. A 404 while
	// creating the folder is about the folder the app syncs into (its root),
	// which is not this session's to give up on: that sync retries as any
	// failure does, and its retry into the folder it made finds out.
	if sess.GDriveFolderID != "" && IsRemoteDeletedError(err) {
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

// folderBack returns nil unless sess is REMOTE_DELETED and Drive does not
// show its folder back out of the trash: then why (still in the trash or
// gone, or Drive did not say).
func (o *Orchestrator) folderBack(ctx context.Context, sess *storage.Session) error {
	if sess.SyncState != storage.SyncStateRemoteDeleted {
		return nil
	}
	back, err := o.uploader.FolderOnDrive(ctx, sess.GDriveFolderID)
	switch {
	case err != nil:
		return fmt.Errorf("cannot sync session %s now: its Drive folder was deleted, and Drive did not say whether it is back: %w: %w", sess.ID, ErrDriveDidNotAnswer, err)
	case !back:
		return fmt.Errorf("cannot sync session %s now: its Drive folder %s is still in the trash or gone; restore it on Drive, then retry", sess.ID, sess.GDriveFolderID)
	}
	return nil
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
