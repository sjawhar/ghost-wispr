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
	// RecordSyncFolder stores a newly created Drive folder while the sync
	// claim still holds the session, before its first upload.
	RecordSyncFolder(sessionID, claim, driveFolderID string) (bool, error)
	// EndSync records how the sync claim names ended, when it did not mark
	// the session synced, only while that sync holds it, and reports whether
	// it did.
	EndSync(sessionID, claim, state, driveFolderID string, retryCount int, at time.Time, errorMessage string) (bool, error)
}

type uploader interface {
	Upload(ctx context.Context, folderName string, files []SyncFile) (string, error)
	UploadInto(ctx context.Context, folderID string, files []SyncFile) error
	FolderOnDrive(ctx context.Context, folderID string) (bool, error)
	RecordingsMatch(ctx context.Context, folderID string, checksums map[string]string) (bool, error)
}

// folderCreatingUploader can record a newly made folder before its first
// upload. Syncer implements it; the base uploader contract stays useful for
// test uploaders that do not create remote folders.
type folderCreatingUploader interface {
	UploadWithFolderRecorded(ctx context.Context, folderName string, files []SyncFile, record func(folderID string) error) (string, error)
}

// sessionFolderFinder lists the Drive folders the app made under its root
// that hold only one session's files, so a sync cut off before it recorded
// its folder can adopt that orphan.
type sessionFolderFinder interface {
	FindSessionFolders(ctx context.Context, sessionID, audioPath string) ([]string, error)
}

var _ folderCreatingUploader = (*Syncer)(nil)
var _ sessionFolderFinder = (*Syncer)(nil)

type legacyFolderRecorder interface {
	RecordLegacyDriveFolders(sessionID, claim string, folderIDs []string) error
}

type replacementInputStore interface {
	DriveReplacementInput(sessionID string) (pending bool, splicedRecording string, err error)
	DeletionGeneration(sessionID string) (int, error)
	ClearDriveReplacementPending(sessionID string, generation int) error
	HasPendingSplice(sessionID string) (bool, error)
	OldestPendingSplice(sessionID string) (time.Time, bool, error)
	// EnsureDriveReplacementFolders reads the deletion's owed folders,
	// rebuilding a ledger that is empty while the deletion is pending.
	EnsureDriveReplacementFolders(sessionID string, generation int) ([]storage.DriveReplacementFolder, error)
	UnrecordedDriveFolderSessions(sessionID string) ([]storage.Session, error)
	// RecordDriveReplacementFailure counts a failed replacement attempt of
	// the session's deletion generation, with why; the app's status warning
	// is read from these counts (storage.OwedDriveReplacements).
	RecordDriveReplacementFailure(sessionID string, generation int, message string) error
}

type Orchestrator struct {
	uploader uploader
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
func (o *Orchestrator) replacementFailed(replacements replacementInputStore, sessionID string, generation int, cause error) {
	if err := replacements.RecordDriveReplacementFailure(sessionID, generation, cause.Error()); err != nil {
		o.logger.Error("gdrive replacement could not record its failure", "operation", "replace_session_files", "session_id", sessionID, "error", err, "replacement_error", cause.Error())
	}
}

func NewOrchestrator(syncer *Syncer, store OrchestratorStore, logger ...*slog.Logger) *Orchestrator {
	l := logging.WithModule(slog.Default(), "gdrive")
	if len(logger) > 0 && logger[0] != nil {
		l = logging.WithModule(logger[0], "gdrive")
	}
	if replacements, ok := store.(replacementStore); ok {
		syncer.setReplacementStore(replacements)
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

// adoptSessionFolder records for the sync claim names an app-made Drive
// folder named folderName that holds only this session's files and that no
// other local session records, and returns its id; "" when there is none to
// adopt (no finder, or no such folder: a same-named folder holding another
// session's files, or recorded by another session, is not this session's
// orphan).
func (o *Orchestrator) adoptSessionFolder(ctx context.Context, sessionID, claim, audioPath string) (string, error) {
	finder, ok := o.uploader.(sessionFolderFinder)
	if !ok {
		return "", nil
	}
	folderIDs, err := finder.FindSessionFolders(ctx, sessionID, audioPath)
	if err != nil {
		return "", fmt.Errorf("find Drive folder for sync %s: %w", sessionID, err)
	}
	for i, folderID := range folderIDs {
		switch err := o.recordSyncFolder(sessionID, claim, folderID); {
		case errors.Is(err, storage.ErrDriveFolderRecorded):
			continue
		case err != nil:
			return "", fmt.Errorf("record existing Drive folder for sync %s: %w", sessionID, err)
		}
		if recorder, ok := o.store.(legacyFolderRecorder); ok && i+1 < len(folderIDs) {
			if err := recorder.RecordLegacyDriveFolders(sessionID, claim, folderIDs[i+1:]); err != nil {
				return "", fmt.Errorf("record additional legacy Drive folders for sync %s: %w", sessionID, err)
			}
		}
		return folderID, nil
	}
	return "", nil
}

// adoptPendingReplacementFolder claims sess long enough to record an orphan
// Drive folder before the replacement ledger decides it has none. It reports
// claimed false when another sync took the session first.
func (o *Orchestrator) adoptPendingReplacementFolder(ctx context.Context, sess *storage.Session) (adopted, claimed bool, err error) {
	if sess.GDriveFolderID != "" || sess.LastSyncAttempt == nil {
		return false, true, nil
	}
	claim := uuid.NewString()
	held, claimed, err := o.store.ClaimSync(sess.ID, sess.SyncState, claim, time.Now().UTC())
	if err != nil || !claimed {
		return false, claimed, err
	}
	folderID, adoptErr := o.adoptSessionFolder(ctx, sess.ID, claim, resolvedAudioPath(sess.AudioPath, o.audioDir))
	message := ""
	if adoptErr != nil {
		message = "folder discovery failed: " + adoptErr.Error()
	}
	released, releaseErr := o.store.EndSync(sess.ID, claim, sess.SyncState, folderID, held.RetryCount, time.Now().UTC(), message)
	if releaseErr != nil {
		return false, true, fmt.Errorf("release folder-discovery claim for session %s: %w", sess.ID, releaseErr)
	}
	if !released {
		return false, false, nil
	}
	if adoptErr != nil {
		return false, true, adoptErr
	}
	return folderID != "", true, nil
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

const spliceWaitWarningAfter = 5 * time.Minute

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
	if replacements, ok := o.store.(replacementInputStore); ok {
		pending, splicedRecording, replacementErr := replacements.DriveReplacementInput(sessionID)
		if replacementErr != nil {
			return replacementErr
		}
		if pending {
			// Read the generation before the snapshot the checks below use, so
			// a deletion committing during them makes the clear refuse.
			generation, err := replacements.DeletionGeneration(sessionID)
			if err != nil {
				return fmt.Errorf("read deletion generation for session %s: %w", sessionID, err)
			}
			if read, err = o.store.GetSession(sessionID); err != nil {
				return fmt.Errorf("get session %s: %w", sessionID, err)
			}
			if spliceOpen, err := replacements.HasPendingSplice(sessionID); err != nil {
				return err
			} else if spliceOpen {
				started, found, err := replacements.OldestPendingSplice(sessionID)
				if err != nil {
					return err
				}
				if found && time.Since(started) >= spliceWaitWarningAfter {
					o.logger.Warn("gdrive replacement splice is taking too long", "operation", "replace_session_files", "session_id", sessionID, "waited", time.Since(started).Round(time.Second), "warning_after", spliceWaitWarningAfter)
				} else {
					o.logger.Info("gdrive replacement waits for the splice", "operation", "replace_session_files", "session_id", sessionID)
				}
				return nil
			}
			if !replacementReady(&read) {
				o.logger.Warn("gdrive replacement waits for refinement and summary", "operation", "replace_session_files", "session_id", sessionID)
				return nil
			}
			unrecorded, err := replacements.UnrecordedDriveFolderSessions(sessionID)
			if err != nil {
				return err
			}
			for i := range unrecorded {
				_, claimed, err := o.adoptPendingReplacementFolder(ctx, &unrecorded[i])
				if err != nil {
					return err
				}
				if !claimed {
					o.logger.Info("gdrive replacement was claimed by another sync first, skipping", "operation", "replace_session_files", "session_id", sessionID)
					return nil
				}
			}
			if read, err = o.store.GetSession(sessionID); err != nil {
				return fmt.Errorf("get session %s after Drive folder discovery: %w", sessionID, err)
			} else if !read.DriveReplacePending {
				return o.SyncSession(ctx, sessionID)
			}
			folders, err := replacements.EnsureDriveReplacementFolders(sessionID, generation)
			if err != nil {
				return err
			}
			if len(folders) == 0 {
				// The deletion owes Drive no folder: none held the session's
				// files, its own or a merged source's, even rebuilt.
				if err := replacements.ClearDriveReplacementPending(sessionID, generation); err != nil {
					return err
				}
				o.logger.Info("gdrive replacement falls through to first sync", "operation", "replace_session_files", "session_id", sessionID)
				return o.SyncSession(ctx, sessionID)
			}
			syncer, ok := o.uploader.(*Syncer)
			if !ok {
				return fmt.Errorf("replace session files %s: the Drive uploader cannot replace files", sessionID)
			}
			return o.replaceSessionFiles(ctx, syncer, replacements, &read, generation, splicedRecording)
		}
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
	syncSess := toSyncSession(&sess)

	// Each recording is uploaded from where every reader of a recording
	// finds it (recording.Resolve); one found nowhere is left out, and
	// notUploaded names it.
	audioPath := resolvedAudioPath(sess.AudioPath, o.audioDir)
	files, folderName, unreadable, err := buildSyncFiles(&syncSess, segments, audioPath)
	if err != nil {
		o.endSync(sessionID, claim, storage.SyncStateFailed, sess.GDriveFolderID, sess.RetryCount, time.Now().UTC(), err)
		return fmt.Errorf("build sync files %s: %w", sessionID, err)
	}

	// A session synced before keeps its folder, and so does one whose first
	// sync created its folder and then failed: syncing it again adds what the
	// folder lacks instead of creating a second folder. A sync cut off after
	// Drive made its folder but before the folder was recorded (or by a build
	// that did not record new folders) left one under the app's Drive root;
	// the retry finds and stores that folder before uploading into it. Only a
	// retry looks: a session no sync attempted has no folder of its own. A
	// retry adopts only a folder holding nothing of another session: a
	// same-named folder can be another session's, one deleted here, say, whose
	// documents an upload would overwrite (FindSessionFolders). A summary the
	// session no longer has is emptied in an adopted folder too.
	driveFolderID := sess.GDriveFolderID
	if driveFolderID == "" && read.LastSyncAttempt != nil {
		driveFolderID, err = o.adoptSessionFolder(ctx, sessionID, claim, audioPath)
		if err != nil {
			err = fmt.Errorf("folder discovery failed: %w", err)
		}
	}
	if err == nil {
		if driveFolderID != "" {
			err = o.uploader.UploadInto(ctx, driveFolderID, withEmptiedSummary(files))
		} else if creator, ok := o.uploader.(folderCreatingUploader); ok {
			driveFolderID, err = creator.UploadWithFolderRecorded(ctx, folderName, files, func(folderID string) error {
			return o.recordSyncFolder(sessionID, claim, folderID)
		})
		} else {
			driveFolderID, err = o.uploader.Upload(ctx, folderName, files)
		}
	}
	if err == nil && sess.GDriveFolderID != "" {
		// Delete text can change the title that names the existing folder.
		// Rename it in place: its id and the links to its files stay.
		if syncer, ok := o.uploader.(*Syncer); ok {
			err = syncer.renameSessionFolder(ctx, driveFolderID, folderName)
		}
	}
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

// replaceSessionFiles replaces the Drive folders sess's segment deletion of
// generation owes, holding the session's sync claim so that retry-sync, the
// sync after a summary and the sweep do not start a second replacement.
//
// It releases the claim, its retry count kept, by what it did to the
// session's own Drive folder, never as synced: a replacement does not prove
// that every other recording of the session is on Drive.
//   - It replaced the session's own folder: PENDING, so the next sync checks
//     Drive against the session and marks it synced. Round 5's rule for an
//     owed session in FAILED or REMOTE_DELETED is this case.
//   - It found the session's own folder in the trash or gone: REMOTE_DELETED,
//     as a sync would leave it (the replacement, still owed for a folder in
//     the trash, is swept until it is restored).
//   - It did not reach the session's own folder (it has none, or an earlier
//     attempt replaced it or found it gone): the state the claim came from,
//     since nothing of the session's own Drive copy changed. A REMOTE_DELETED
//     session stays REMOTE_DELETED.
func (o *Orchestrator) replaceSessionFiles(ctx context.Context, syncer *Syncer, replacements replacementInputStore, sess *storage.Session, generation int, splicedRecording string) error {
	sessionID := sess.ID
	claim := uuid.NewString()
	claimed, ok, err := o.store.ClaimSync(sessionID, sess.SyncState, claim, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("claim replacement sync of %s: %w", sessionID, err)
	}
	if !ok {
		o.logger.Info("gdrive replacement was claimed by another sync first, skipping", "operation", "replace_session_files", "session_id", sessionID)
		return nil
	}
	outcome, replaceErr := syncer.replaceSessionFiles(ctx, &claimed, splicedRecording)
	release := syncStateOf(sess)
	switch {
	case outcome.ownTrashed || outcome.ownGone:
		release = storage.SyncStateRemoteDeleted
	case outcome.ownReplaced, release == storage.SyncStateSynced:
		release = storage.SyncStatePending
	}
	message := ""
	if replaceErr != nil {
		message = replaceErr.Error()
	}
	released, releaseErr := o.store.EndSync(sessionID, claim, release, claimed.GDriveFolderID, claimed.RetryCount, time.Now().UTC(), message)
	switch {
	case releaseErr != nil:
		o.logger.Error("gdrive replacement could not release its sync claim; the session stays mid-sync until the app restarts", "operation", "replace_session_files", "session_id", sessionID, "error", releaseErr)
		if replaceErr == nil {
			replaceErr = fmt.Errorf("release replacement sync claim: %w", releaseErr)
		}
	case !released:
		o.logger.Warn("gdrive replacement no longer holds its session; its state is left as it is", "operation", "replace_session_files", "session_id", sessionID)
	}
	if replaceErr != nil {
		o.replacementFailed(replacements, sessionID, generation, replaceErr)
		return fmt.Errorf("replace session files %s: %w", sessionID, replaceErr)
	}
	o.logger.Info("gdrive session files replaced", "operation", "replace_session_files", "session_id", sessionID, "drive_folder_id", claimed.GDriveFolderID, "sync_state", release)
	return nil
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
