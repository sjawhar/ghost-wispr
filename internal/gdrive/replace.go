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

	"google.golang.org/api/drive/v3"

	recordingname "github.com/sjawhar/ghost-wispr/internal/recording"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

type replacementStore interface {
	GetSession(id string) (storage.Session, error)
	DriveReplacements(sessionID string, generation int) ([]storage.DriveReplacement, error)
	PendingDriveReplacementRecordings(sessionID string) ([]string, error)
	StartDriveReplacement(sessionID, name string, generation int, oldIDs []string) error
	MarkDriveReplacementUploaded(sessionID, name string, generation int, newID string) error
	MarkDriveReplacementDeleted(sessionID, name, oldID string, generation int) error
	DeletionGeneration(sessionID string) (int, error)
	ClearDriveReplacementPending(sessionID string, generation int) error
	EnsureDriveReplacementFolders(sessionID string, generation int) ([]storage.DriveReplacementFolder, error)
	MarkDriveReplacementFolder(sessionID string, generation int, folderSessionID, state string) error
	MarkLegacyDriveReplacementFolder(sessionID string, generation int, folderSessionID, folderID, state string) error
	FinishDriveReplacement(sessionID string, generation int) error
}

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

func (s *Syncer) setReplacementStore(store replacementStore) {
	s.replacements = store
}

// ReplaceSessionFiles replaces, in every Drive folder that can hold a deleted
// segment's words (the session's own, and each folder of a session merged
// into it, as the deletion recorded them owed), transcript.md, summary.md and
// each spliced recording that folder has. See replaceSessionFiles.
//
//nolint:gocritic // The approved API accepts storage.Session by value.
func (s *Syncer) ReplaceSessionFiles(ctx context.Context, sess storage.Session, splicedRecording string) error {
	_, err := s.replaceSessionFiles(ctx, &sess, splicedRecording)
	return err
}

// replaceSessionFiles replaces every Drive folder sess's segment deletion
// still owes that Drive can reach now, one folder at a time: it uploads and
// verifies the folder's replacements, recording each new id before it
// deletes any old id there (so a retry resumes from the rows and never
// deletes the replacement it chose), deletes the folder's old files, renames
// the folder and records it replaced. A folder gone for good is recorded
// gone, its old files gone with it. A folder in the trash stays owed: the
// attempt fails, and the sweep comes back until the folder is restored. The
// folders Drive can reach are replaced meanwhile, so one in the trash does not
// keep the words in the others. The deletion finishes once no folder is owed.
func (s *Syncer) replaceSessionFiles(ctx context.Context, sess *storage.Session, splicedRecording string) (replacementOutcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var outcome replacementOutcome
	if s.replacements == nil {
		return outcome, fmt.Errorf("replace session files %s: replacement store is required", sess.ID)
	}
	generation, err := s.replacements.DeletionGeneration(sess.ID)
	if err != nil {
		return outcome, fmt.Errorf("read deletion generation for session %s: %w", sess.ID, err)
	}
	current, err := s.replacements.GetSession(sess.ID)
	if err != nil {
		return outcome, fmt.Errorf("read replacement session %s: %w", sess.ID, err)
	}
	if !current.DriveReplacePending || !replacementReady(&current) {
		return outcome, nil
	}
	recordings, err := s.replacements.PendingDriveReplacementRecordings(current.ID)
	if err != nil {
		return outcome, fmt.Errorf("list replacement recordings for session %s: %w", current.ID, err)
	}
	if len(recordings) == 0 {
		recordings = []string{splicedRecording}
	}
	folders, err := s.replacements.EnsureDriveReplacementFolders(current.ID, generation)
	if err != nil {
		return outcome, err
	}
	if len(folders) == 0 {
		// The deletion owes no folder: none held the session's files.
		return outcome, s.replacements.ClearDriveReplacementPending(current.ID, generation)
	}

	var failures []error
	for _, folder := range folders {
		if folder.State != storage.DriveReplacementFolderOwed {
			continue
		}
		own := folder.FolderSessionID == current.ID
		folderSession := current
		if !own {
			if folderSession, err = s.replacements.GetSession(folder.FolderSessionID); err != nil {
				failures = append(failures, fmt.Errorf("read merged session %s: %w", folder.FolderSessionID, err))
				continue
			}
		}
		if folder.Legacy {
			folderSession.GDriveFolderID = folder.FolderID
		}
		if err := s.checkFolder(ctx, folderSession.GDriveFolderID); err != nil {
			switch {
			case errors.Is(err, ErrFolderTrashed):
				outcome.ownTrashed = outcome.ownTrashed || own
				failures = append(failures, fmt.Errorf("the Drive folder of session %s is in the trash, and its replacement runs once it is restored: %w", folderSession.ID, err))
			case IsRemoteDeletedError(err):
				if folder.Legacy {
					err = s.replacements.MarkLegacyDriveReplacementFolder(current.ID, generation, folderSession.ID, folder.FolderID, storage.DriveReplacementFolderGone)
				} else {
					err = s.replacements.MarkDriveReplacementFolder(current.ID, generation, folderSession.ID, storage.DriveReplacementFolderGone)
				}
				if err != nil {
					failures = append(failures, err)
					continue
				}
				outcome.ownGone = outcome.ownGone || own
			default:
				failures = append(failures, fmt.Errorf("check the Drive folder of session %s: %w", folderSession.ID, err))
			}
			continue
		}
		legacyFolderID := ""
		if folder.Legacy {
			legacyFolderID = folder.FolderID
		}
		if err := s.replaceFolder(ctx, current.ID, generation, &folderSession, recordings, own, legacyFolderID); err != nil {
			failures = append(failures, err)
			continue
		}
		outcome.ownReplaced = outcome.ownReplaced || own
	}
	if len(failures) != 0 {
		return outcome, errors.Join(failures...)
	}
	return outcome, s.replacements.FinishDriveReplacement(current.ID, generation)
}

// replaceFolder replaces the Drive folder of folderSession, which ownerID's
// segment deletion at generation owes: uploads and verifies its replacements,
// deletes its old files and duplicates, renames it and records it replaced.
// A merged source's folder (own false) gets the spliced recordings it holds,
// under their own names, and its scrubbed transcript and summary.
func (s *Syncer) replaceFolder(ctx context.Context, ownerID string, generation int, folderSession *storage.Session, recordings []string, own bool, folderID string) error {
	target := &replacementTarget{
		session:    *folderSession,
		folderName: folderSession.StartedAt.UTC().Format("2006-01-02") + "-" + slugify(folderSession.Title),
	}
	var err error
	if target.rows, err = s.replacements.DriveReplacements(target.session.ID, generation); err != nil {
		return err
	}
	if !allDriveReplacementsDeleted(target.rows) {
		if target.existing, err = s.ListFolderFiles(ctx, target.session.GDriveFolderID); err != nil {
			return fmt.Errorf("list replacements for session %s: %w", target.session.ID, err)
		}
		folderRecordings := recordings
		if !own {
			folderRecordings = recordingsInFolder(recordings, target.existing, target.rows)
		}
		if target.files, _, err = replacementFiles(&target.session, folderRecordings); err != nil {
			return fmt.Errorf("build replacements for session %s: %w", target.session.ID, err)
		}
	}
	if err := s.uploadReplacements(ctx, target, generation); err != nil {
		return err
	}
	deleteFile := func(fileID string) error {
		err := s.service.Files.Delete(fileID).SupportsAllDrives(true).Context(ctx).Do()
		if err != nil && !IsRemoteDeletedError(err) {
			return err
		}
		return nil
	}
	if err := s.deleteReplacedFiles(ctx, ownerID, target, generation, deleteFile); err != nil {
		return err
	}
	if err := s.renameSessionFolder(ctx, target.session.GDriveFolderID, target.folderName); err != nil {
		return err
	}
	if folderID != "" {
		return s.replacements.MarkLegacyDriveReplacementFolder(ownerID, generation, target.session.ID, folderID, storage.DriveReplacementFolderReplaced)
	}
	return s.replacements.MarkDriveReplacementFolder(ownerID, generation, target.session.ID, storage.DriveReplacementFolderReplaced)
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
func (s *Syncer) uploadReplacements(ctx context.Context, target *replacementTarget, generation int) error {
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
			if err := s.replacements.StartDriveReplacement(id, file.Name, generation, driveFileIDs(oldByName[file.Name])); err != nil {
				return err
			}
			if target.rows, err = s.replacements.DriveReplacements(id, generation); err != nil {
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
			if newID, err = s.uploadFileID(ctx, folder, file); err != nil {
				return err
			}
		} else {
			newID = newFile.Id
		}
		if err := s.verifyFile(ctx, newID, file); err != nil {
			return fmt.Errorf("verify replacement %s: %w", file.Name, err)
		}
		if err := s.replacements.MarkDriveReplacementUploaded(id, file.Name, generation, newID); err != nil {
			return err
		}
		if target.rows, err = s.replacements.DriveReplacements(id, generation); err != nil {
			return err
		}
	}
	if target.rows, err = s.replacements.DriveReplacements(id, generation); err != nil {
		return err
	}
	if !hasReplacementForEveryFile(target.rows, target.files) {
		return fmt.Errorf("replace session files %s: replacements are incomplete", id)
	}
	return nil
}

// deleteReplacedFiles deletes, with deleteFile, target's duplicate replacement
// uploads and then each old file its rows record, while the deletion
// generation of ownerID (the session the segment was deleted from) is still
// generation.
func (s *Syncer) deleteReplacedFiles(ctx context.Context, ownerID string, target *replacementTarget, generation int, deleteFile func(fileID string) error) error {
	if target.files == nil {
		return nil
	}
	id := target.session.ID
	// Every copy of a replacement this app uploaded, other than the one its
	// rows record, is a duplicate: an upload Drive carried out twice because
	// a retried request's first response was lost, in an earlier attempt or
	// in this one. The folder is listed again after the uploads, so a copy
	// made in this attempt is found too.
	uploaded, err := s.ListFolderFiles(ctx, target.session.GDriveFolderID)
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
			if err := deleteFile(duplicateID); err != nil {
				return fmt.Errorf("delete duplicate replacement upload %s: %w", duplicateID, err)
			}
		}
	}
	for _, replacement := range target.rows {
		switch replacement.State {
		case storage.DriveReplacementDeleted:
			continue
		case storage.DriveReplacementUploaded:
			currentGeneration, err := s.replacements.DeletionGeneration(ownerID)
			if err != nil {
				return fmt.Errorf("read deletion generation for session %s: %w", ownerID, err)
			}
			if currentGeneration != generation {
				return fmt.Errorf("replace session files %s: deletion generation changed from %d to %d", ownerID, generation, currentGeneration)
			}
			if replacement.OldID != "" {
				if err := deleteFile(replacement.OldID); err != nil {
					return fmt.Errorf("delete old %s: %w", replacement.Name, err)
				}
			}
			if err := s.replacements.MarkDriveReplacementDeleted(id, replacement.Name, replacement.OldID, generation); err != nil {
				return err
			}
		default:
			return fmt.Errorf("replace session files %s: replacement %s has state %q", id, replacement.Name, replacement.State)
		}
	}
	return nil
}

func replacementFiles(sess *storage.Session, recordings []string) ([]SyncFile, string, error) {
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
		hash, err := fileSHA256(recording)
		if err != nil {
			return nil, "", fmt.Errorf("hash spliced recording %s: %w", recording, err)
		}
		mimeType := "audio/mpeg"
		if filepath.Ext(recording) == ".wav" {
			mimeType = "audio/wav"
		}
		files = append(files, SyncFile{Name: recordingname.FileName(recording), MimeType: mimeType, LocalPath: recording, ContentType: mimeType, SHA256: hash})
	}
	return files, sess.StartedAt.UTC().Format("2006-01-02") + "-" + slugify(sess.Title), nil
}

func (s *Syncer) verifyFile(ctx context.Context, fileID string, expected *SyncFile) error {
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

func (s *Syncer) renameSessionFolder(ctx context.Context, folderID, name string) error {
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
