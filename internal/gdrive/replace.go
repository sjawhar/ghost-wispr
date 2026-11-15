package gdrive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/google/uuid"
	"google.golang.org/api/drive/v3"

	recordingname "github.com/sjawhar/ghost-wispr/internal/recording"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

const spliceWaitWarningAfter = 5 * time.Minute

// replacementTarget is one Drive folder a replacement covers: the session's
// own, or that of a session merged into it, which the merge left on Drive
// with its own copies of the transcript, summary and recording. Its rows in
// drive_replacements are under its own session id and the deleting session's
// generation.
type replacementTarget struct {
	session    storage.Session
	folderName string
	// files is what the folder gets; nil once every row of the target is
	// deleted, when only the folder name and the finish are left.
	files    []SyncFile
	existing []*drive.File
	rows     []storage.DriveReplacement
}

// replacementOutcome is what one replacement attempt did to the session's
// own Drive folder: replaced it, or found it in the trash or gone. All false:
// the session has no folder of its own, or this attempt did not reach it
// (an earlier one did).
type replacementOutcome struct {
	ownReplaced, ownTrashed, ownGone bool
}

// replaceOwedFiles is a sync of session sessionID while its segment deletion
// still owes Drive a replacement: it waits for the splice, then for
// refinement and summary, then replaces the folders the deletion owes. A
// deletion that owes no folder is cleared, and the session synced as a first
// sync.
func (o *Orchestrator) replaceOwedFiles(ctx context.Context, sessionID, splicedRecording string) error {
	// Read the generation before the snapshot the checks below use, so a
	// deletion committing during them makes the clear and the finish refuse.
	generation, err := o.store.DeletionGeneration(sessionID)
	if err != nil {
		return fmt.Errorf("read deletion generation for session %s: %w", sessionID, err)
	}
	read, err := o.store.GetSession(sessionID)
	if err != nil {
		return fmt.Errorf("get session %s: %w", sessionID, err)
	}
	if spliceOpen, err := o.store.HasPendingSplice(sessionID); err != nil {
		return err
	} else if spliceOpen {
		started, found, err := o.store.OldestPendingSplice(sessionID)
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
	folders, err := o.store.EnsureDriveReplacementFolders(sessionID, generation)
	if err != nil {
		return err
	}
	if len(folders) == 0 {
		// The deletion owes Drive no folder: none held the session's
		// files, its own or a merged source's, even rebuilt.
		if err := o.store.ClearDriveReplacementPending(sessionID, generation); err != nil {
			return err
		}
		o.logger.Info("gdrive replacement falls through to first sync", "operation", "replace_session_files", "session_id", sessionID)
		return o.SyncSession(ctx, sessionID)
	}
	return o.replaceSessionFiles(ctx, &read, generation, folders, splicedRecording)
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
func (o *Orchestrator) replaceSessionFiles(ctx context.Context, sess *storage.Session, generation int, folders []storage.DriveReplacementFolder, splicedRecording string) error {
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
	var outcome replacementOutcome
	replaceErr := o.uploader.Serialize(func() error {
		current, replacing, owed, err := o.replacementUnderLock(sessionID, generation, folders)
		if err != nil || current == nil {
			return err
		}
		outcome, err = o.replaceOwedFolders(ctx, current, replacing, owed, splicedRecording)
		return err
	})
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
		o.replacementFailed(sessionID, generation, replaceErr)
		return fmt.Errorf("replace session files %s: %w", sessionID, replaceErr)
	}
	o.logger.Info("gdrive session files replaced", "operation", "replace_session_files", "session_id", sessionID, "drive_folder_id", claimed.GDriveFolderID, "sync_state", release)
	return nil
}

// replacementUnderLock reads session sessionID's deletion generation and the
// session again once the replacement has the Drive lock, which it can wait
// for as long as an upload of another session takes, and returns the
// session, generation and folders to replace, or a nil session when there is
// nothing to replace now. When no deletion committed during the wait, that is
// the fresh session with generation and folders, as the checks before the
// claim read them. When one did, it replaces for that newer deletion, as the
// replacement did before the gdrive refactor: if the fresh session is pending
// and ready (refined and summarized again), it ensures the newer
// generation's folders, clearing the pending flag when the deletion owes
// none; otherwise it replaces nothing, and a later sync does.
func (o *Orchestrator) replacementUnderLock(sessionID string, generation int, folders []storage.DriveReplacementFolder) (*storage.Session, int, []storage.DriveReplacementFolder, error) {
	now, err := o.store.DeletionGeneration(sessionID)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("read deletion generation for session %s: %w", sessionID, err)
	}
	current, err := o.store.GetSession(sessionID)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("read replacement session %s: %w", sessionID, err)
	}
	if now == generation {
		return &current, generation, folders, nil
	}
	if !current.DriveReplacePending || !replacementReady(&current) {
		o.logger.Info("gdrive replacement waits for a newer deletion's refinement and summary", "operation", "replace_session_files", "session_id", sessionID, "generation", generation, "newer_generation", now)
		return nil, 0, nil, nil
	}
	if folders, err = o.store.EnsureDriveReplacementFolders(sessionID, now); err != nil {
		return nil, 0, nil, err
	}
	if len(folders) == 0 {
		// The newer deletion owes Drive no folder.
		return nil, 0, nil, o.store.ClearDriveReplacementPending(sessionID, now)
	}
	return &current, now, folders, nil
}

// replaceOwedFolders replaces each of folders that current's segment
// deletion of generation still owes and Drive can reach now, one folder at a
// time: it uploads and verifies the folder's replacements, recording each new
// id before it deletes any old id there (so a retry resumes from the rows and
// never deletes the replacement it chose), deletes the folder's old files,
// renames the folder and records it replaced. A folder gone for good is
// recorded gone, its old files gone with it. A folder in the trash stays
// owed: the attempt fails, and the sweep comes back until the folder is
// restored. The folders Drive can reach are replaced meanwhile, so one in the
// trash does not keep the words in the others. The deletion finishes once no
// folder is owed. current is the session read under the Drive lock
// (replacementUnderLock): one whose replacement finished, or whose
// deletion is not yet refined and summarized again, replaces nothing.
func (o *Orchestrator) replaceOwedFolders(ctx context.Context, current *storage.Session, generation int, folders []storage.DriveReplacementFolder, splicedRecording string) (replacementOutcome, error) {
	var outcome replacementOutcome
	if !current.DriveReplacePending || !replacementReady(current) {
		return outcome, nil
	}
	recordings, err := o.store.PendingDriveReplacementRecordings(current.ID)
	if err != nil {
		return outcome, fmt.Errorf("list replacement recordings for session %s: %w", current.ID, err)
	}
	if len(recordings) == 0 {
		recordings = []string{splicedRecording}
	}

	var failures []error
	for _, folder := range folders {
		if folder.State != storage.DriveReplacementFolderOwed {
			continue
		}
		own := folder.FolderSessionID == current.ID
		folderSession := *current
		if !own {
			if folderSession, err = o.store.GetSession(folder.FolderSessionID); err != nil {
				failures = append(failures, fmt.Errorf("read merged session %s: %w", folder.FolderSessionID, err))
				continue
			}
		}
		if err := o.uploader.CheckFolder(ctx, folderSession.GDriveFolderID); err != nil {
			switch {
			case errors.Is(err, ErrFolderTrashed):
				outcome.ownTrashed = outcome.ownTrashed || own
				failures = append(failures, fmt.Errorf("the Drive folder of session %s is in the trash, and its replacement runs once it is restored: %w", folderSession.ID, err))
			case IsRemoteDeletedError(err):
				if err := o.store.MarkDriveReplacementFolder(current.ID, generation, folderSession.ID, storage.DriveReplacementFolderGone); err != nil {
					failures = append(failures, err)
					continue
				}
				outcome.ownGone = outcome.ownGone || own
			default:
				failures = append(failures, fmt.Errorf("check the Drive folder of session %s: %w", folderSession.ID, err))
			}
			continue
		}
		if err := o.replaceFolder(ctx, current.ID, generation, &folderSession, recordings, own); err != nil {
			failures = append(failures, err)
			continue
		}
		outcome.ownReplaced = outcome.ownReplaced || own
	}
	if len(failures) != 0 {
		return outcome, errors.Join(failures...)
	}
	return outcome, o.store.FinishDriveReplacement(current.ID, generation)
}

// replaceFolder replaces the Drive folder of folderSession, which ownerID's
// segment deletion at generation owes: uploads and verifies its replacements,
// deletes its old files and duplicates, renames it and records it replaced.
// A merged source's folder (own false) gets the spliced recordings it holds,
// under their own names, and its scrubbed transcript and summary.
func (o *Orchestrator) replaceFolder(ctx context.Context, ownerID string, generation int, folderSession *storage.Session, recordings []string, own bool) error {
	target := &replacementTarget{
		session:    *folderSession,
		folderName: folderSession.StartedAt.UTC().Format("2006-01-02") + "-" + slugify(folderSession.Title),
	}
	var err error
	if target.rows, err = o.store.DriveReplacements(target.session.ID, generation); err != nil {
		return err
	}
	if !allDriveReplacementsDeleted(target.rows) {
		if target.existing, err = o.uploader.ListFolderFiles(ctx, target.session.GDriveFolderID); err != nil {
			return fmt.Errorf("list replacements for session %s: %w", target.session.ID, err)
		}
		folderRecordings := recordings
		if !own {
			folderRecordings = recordingsInFolder(recordings, target.existing, target.rows)
		}
		if target.files, _, err = replacementFiles(ownerID, &target.session, folderRecordings, o.store); err != nil {
			return fmt.Errorf("build replacements for session %s: %w", target.session.ID, err)
		}
	}
	if err := o.uploadReplacements(ctx, target, generation); err != nil {
		return err
	}
	if err := o.deleteReplacedFiles(ctx, ownerID, target, generation); err != nil {
		return err
	}
	if err := o.uploader.RenameFolder(ctx, target.session.GDriveFolderID, target.folderName); err != nil {
		return err
	}
	return o.store.MarkDriveReplacementFolder(ownerID, generation, target.session.ID, storage.DriveReplacementFolderReplaced)
}

// recordingsInFolder returns the recordings whose Drive copy (audio- and the
// file name) the folder listed in existing holds, or that rows already
// record a replacement of.
func recordingsInFolder(recordings []string, existing []*drive.File, rows []storage.DriveReplacement) []string {
	held := map[string]bool{}
	for _, file := range existing {
		held[file.Name] = true
	}
	for _, row := range rows {
		held[row.Name] = true
	}
	var in []string
	for _, recording := range recordings {
		if held[recordingname.FileName(recording)] {
			in = append(in, recording)
		}
	}
	return in
}

// uploadReplacements uploads and verifies target's replacement files and
// records each new id, resuming from the rows of an earlier attempt.
func (o *Orchestrator) uploadReplacements(ctx context.Context, target *replacementTarget, generation int) error {
	if target.files == nil {
		return nil
	}
	id, folder := target.session.ID, target.session.GDriveFolderID
	oldByName := make(map[string][]*drive.File, len(target.files))
	for _, file := range target.existing {
		for i := range target.files {
			if file.Name == target.files[i].Name {
				oldByName[file.Name] = append(oldByName[file.Name], file)
			}
		}
	}
	var err error
	for i := range target.files {
		file := &target.files[i]
		rows := driveReplacementsNamed(target.rows, file.Name)
		if len(rows) == 0 {
			if err := o.store.StartDriveReplacement(id, file.Name, generation, driveFileIDs(oldByName[file.Name])); err != nil {
				return err
			}
			if target.rows, err = o.store.DriveReplacements(id, generation); err != nil {
				return err
			}
			rows = driveReplacementsNamed(target.rows, file.Name)
		}
		if !driveReplacementIsUploading(rows) {
			continue
		}
		key := replacementUploadKey(id, file.Name, rows)
		newFile, err := replacementFileForKey(target.existing, file.Name, key)
		if err != nil {
			return err
		}
		if file.SHA256 == "" {
			file.SHA256 = contentHash(file)
		}
		newID := ""
		if newFile == nil {
			file.AppProperties = map[string]string{replacementKeyProperty: key}
			if newID, err = o.uploader.UploadFile(ctx, folder, file); err != nil {
				return err
			}
		} else {
			newID = newFile.Id
		}
		if err := o.uploader.VerifyFile(ctx, newID, file); err != nil {
			return fmt.Errorf("verify replacement %s: %w", file.Name, err)
		}
		if err := o.store.MarkDriveReplacementUploaded(id, file.Name, generation, newID); err != nil {
			return err
		}
		if target.rows, err = o.store.DriveReplacements(id, generation); err != nil {
			return err
		}
	}
	if target.rows, err = o.store.DriveReplacements(id, generation); err != nil {
		return err
	}
	if !hasReplacementForEveryFile(target.rows, target.files) {
		return fmt.Errorf("replace session files %s: replacements are incomplete", id)
	}
	return nil
}

// deleteReplacedFiles deletes target's duplicate replacement uploads and
// then each old file its rows record, while the deletion generation of
// ownerID (the session the segment was deleted from) is still generation.
func (o *Orchestrator) deleteReplacedFiles(ctx context.Context, ownerID string, target *replacementTarget, generation int) error {
	if target.files == nil {
		return nil
	}
	id := target.session.ID
	// Every copy of a replacement this app uploaded, other than the one its
	// rows record, is a duplicate: an upload Drive carried out twice because
	// a retried request's first response was lost, in an earlier attempt or
	// in this one. The folder is listed again after the uploads, so a copy
	// made in this attempt is found too.
	uploaded, err := o.uploader.ListFolderFiles(ctx, target.session.GDriveFolderID)
	if err != nil {
		return fmt.Errorf("list replacements for session %s after uploading: %w", id, err)
	}
	for i := range target.files {
		rows := driveReplacementsNamed(target.rows, target.files[i].Name)
		if len(rows) == 0 || rows[0].NewID == "" {
			continue
		}
		key := replacementUploadKey(id, target.files[i].Name, rows)
		for _, duplicateID := range extraReplacementFileIDs(uploaded, target.files[i].Name, key, rows[0].NewID) {
			if err := o.uploader.DeleteFile(ctx, duplicateID); err != nil {
				return fmt.Errorf("delete duplicate replacement upload %s: %w", duplicateID, err)
			}
		}
	}
	for _, replacement := range target.rows {
		switch replacement.State {
		case storage.DriveReplacementDeleted:
			continue
		case storage.DriveReplacementUploaded:
			currentGeneration, err := o.store.DeletionGeneration(ownerID)
			if err != nil {
				return fmt.Errorf("read deletion generation for session %s: %w", ownerID, err)
			}
			if currentGeneration != generation {
				return fmt.Errorf("replace session files %s: deletion generation changed from %d to %d", ownerID, generation, currentGeneration)
			}
			if replacement.OldID != "" {
				if err := o.uploader.DeleteFile(ctx, replacement.OldID); err != nil {
					return fmt.Errorf("delete old %s: %w", replacement.Name, err)
				}
			}
			if err := o.store.MarkDriveReplacementDeleted(id, replacement.Name, replacement.OldID, generation); err != nil {
				return err
			}
		default:
			return fmt.Errorf("replace session files %s: replacement %s has state %q", id, replacement.Name, replacement.State)
		}
	}
	return nil
}

func replacementFiles(ownerID string, sess *storage.Session, recordings []string, checksums interface {
	LatestRecordingSHA256(sessionID, recording string) (string, bool, error)
}) ([]SyncFile, string, error) {
	syncSession := toSyncSession(sess)
	summary, transcript := renderDocuments(&syncSession, nil)
	if sess.SummaryStatus != storage.SummaryCompleted {
		summary = ""
	}
	files := []SyncFile{
		{Name: "summary.md", MimeType: "application/vnd.google-apps.document", Content: []byte(summary), ContentType: "text/plain"},
		{Name: "transcript.md", MimeType: "application/vnd.google-apps.document", Content: []byte(transcript), ContentType: "text/plain"},
	}
	for _, recording := range recordings {
		hash, err := recordingname.SHA256(recording)
		if err != nil {
			return nil, "", fmt.Errorf("hash spliced recording %s: %w", recording, err)
		}
		expected, cut, err := checksums.LatestRecordingSHA256(ownerID, recording)
		if err != nil {
			return nil, "", fmt.Errorf("read latest cut hash for recording %s: %w", recording, err)
		}
		if cut && hash != expected {
			return nil, "", fmt.Errorf("refuse replacement recording %s: SHA-256 %q does not match the latest cut hash %q", recording, hash, expected)
		}
		mimeType := "audio/mpeg"
		if filepath.Ext(recording) == ".wav" {
			mimeType = "audio/wav"
		}
		files = append(files, SyncFile{Name: recordingname.FileName(recording), MimeType: mimeType, LocalPath: recording, ContentType: mimeType, SHA256: hash})
	}
	return files, sess.StartedAt.UTC().Format("2006-01-02") + "-" + slugify(sess.Title), nil
}

// The Drive file operations a replacement runs (replacementDrive). The
// Orchestrator runs them under Serialize, which holds the same lock as an
// upload, so a replacement never runs beside an upload of this Syncer.

// Serialize runs fn while no upload or other replacement of this Syncer
// runs.
func (s *Syncer) Serialize(fn func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fn()
}

// DeleteFile deletes Drive file fileID; one already gone is no error.
func (s *Syncer) DeleteFile(ctx context.Context, fileID string) error {
	err := s.service.Files.Delete(fileID).SupportsAllDrives(true).Context(ctx).Do()
	if err != nil && !IsRemoteDeletedError(err) {
		return err
	}
	return nil
}

// VerifyFile checks that Drive file fileID has expected's content hash and,
// for a local file, its SHA-256 and size.
func (s *Syncer) VerifyFile(ctx context.Context, fileID string, expected *SyncFile) error {
	file, err := s.service.Files.Get(fileID).SupportsAllDrives(true).Context(ctx).
		Fields("id, appProperties, sha256Checksum, size").Do()
	if err != nil {
		return fmt.Errorf("read uploaded file %s: %w", fileID, err)
	}
	if got, want := file.AppProperties[contentHashProperty], expected.SHA256; got != want {
		return fmt.Errorf("uploaded file %s hash = %q, want %q", fileID, got, want)
	}
	if expected.LocalPath == "" {
		return nil
	}
	info, err := os.Stat(expected.LocalPath)
	if err != nil {
		return fmt.Errorf("stat uploaded source %s: %w", expected.LocalPath, err)
	}
	if file.Sha256Checksum != expected.SHA256 || file.Size != info.Size() {
		return fmt.Errorf("uploaded file %s content differs: sha256=%q size=%d, want sha256=%q size=%d",
			fileID, file.Sha256Checksum, file.Size, expected.SHA256, info.Size())
	}
	return nil
}

// RenameFolder renames Drive folder folderID to name; its id and the links
// to its files stay.
func (s *Syncer) RenameFolder(ctx context.Context, folderID, name string) error {
	if _, err := s.service.Files.Update(folderID, &drive.File{Name: name}).SupportsAllDrives(true).Context(ctx).Fields("id").Do(); err != nil {
		return fmt.Errorf("rename session folder %s: %w", folderID, err)
	}
	return nil
}

func allDriveReplacementsDeleted(replacements []storage.DriveReplacement) bool {
	if len(replacements) == 0 {
		return false
	}
	for _, replacement := range replacements {
		if replacement.State != storage.DriveReplacementDeleted {
			return false
		}
	}
	return true
}

func hasReplacementForEveryFile(replacements []storage.DriveReplacement, files []SyncFile) bool {
	seen := make(map[string]bool, len(files))
	for _, replacement := range replacements {
		seen[replacement.Name] = true
	}
	for _, file := range files {
		if !seen[file.Name] {
			return false
		}
	}
	return true
}

const replacementKeyProperty = "ghostWisprReplacementKey"

func driveReplacementsNamed(replacements []storage.DriveReplacement, name string) []storage.DriveReplacement {
	var named []storage.DriveReplacement
	for _, replacement := range replacements {
		if replacement.Name == name {
			named = append(named, replacement)
		}
	}
	return named
}

func driveReplacementIsUploading(replacements []storage.DriveReplacement) bool {
	for _, replacement := range replacements {
		if replacement.State == storage.DriveReplacementUploading {
			return true
		}
	}
	return false
}

func driveFileIDs(files []*drive.File) []string {
	ids := make([]string, 0, len(files))
	for _, file := range files {
		ids = append(ids, file.Id)
	}
	return ids
}

func replacementUploadKey(sessionID, name string, replacements []storage.DriveReplacement) string {
	ids := make([]string, 0, len(replacements))
	for _, replacement := range replacements {
		ids = append(ids, replacement.OldID)
	}
	sort.Strings(ids)
	hash := sha256.New()
	for _, value := range append([]string{sessionID, name}, ids...) {
		_, _ = hash.Write([]byte(value))
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// replacementFileForKey returns the newest copy of name in files that carries
// replacement key key, or nil. Drive's modified times are compared as times,
// not as text, which would order fractional seconds wrongly.
func replacementFileForKey(files []*drive.File, name, key string) (*drive.File, error) {
	var found *drive.File
	var foundAt time.Time
	for _, file := range files {
		if file.Name != name || file.AppProperties[replacementKeyProperty] != key {
			continue
		}
		if at := toDriveFile(file).ModifiedTime; found == nil || at.After(foundAt) {
			found, foundAt = file, at
		}
	}
	return found, nil
}

func extraReplacementFileIDs(files []*drive.File, name, key, keepID string) []string {
	var ids []string
	for _, file := range files {
		if file.Name == name && file.Id != keepID && file.AppProperties[replacementKeyProperty] == key {
			ids = append(ids, file.Id)
		}
	}
	return ids
}
