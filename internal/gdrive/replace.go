package gdrive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"google.golang.org/api/drive/v3"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

type replacementStore interface {
	GetSession(id string) (storage.Session, error)
	DriveReplacements(sessionID string, generation ...int) ([]storage.DriveReplacement, error)
	PendingDriveReplacementRecordings(sessionID string) ([]string, error)
	StartDriveReplacement(sessionID, name string, generation int, oldIDs []string) error
	MarkDriveReplacementUploaded(sessionID, name string, generation int, newID string) error
	MarkDriveReplacementDeleted(sessionID, name, oldID string, generation int) error
	DeletionGeneration(sessionID string) (int, error)
	FinishDriveReplacement(sessionID string, generation int) error
}

func (s *Syncer) setReplacementStore(store replacementStore) {
	s.replacements = store
}

// ReplaceSessionFiles replaces exactly three files: transcript.md, summary.md
// and the one recording that was spliced. It records every new id before it
// deletes an old id, so a retry resumes from the rows and never deletes a new
// file or uploads an already recorded replacement.
//
//nolint:gocritic // The approved API accepts storage.Session by value.
func (s *Syncer) ReplaceSessionFiles(ctx context.Context, sess storage.Session, splicedRecording string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.replacements == nil {
		return fmt.Errorf("replace session files %s: replacement store is required", sess.ID)
	}
	current, err := s.replacements.GetSession(sess.ID)
	if err != nil {
		return fmt.Errorf("read replacement session %s: %w", sess.ID, err)
	}
	sess = current
	if !sess.DriveReplacePending || !replacementReady(&sess) {
		return nil
	}
	if sess.GDriveFolderID == "" {
		return fmt.Errorf("replace session files %s: session has no Drive folder", sess.ID)
	}

	generation, err := s.replacements.DeletionGeneration(sess.ID)
	if err != nil {
		return fmt.Errorf("read deletion generation for session %s: %w", sess.ID, err)
	}

	replacements, err := s.replacements.DriveReplacements(sess.ID, generation)
	if err != nil {
		return err
	}
	if allDriveReplacementsDeleted(replacements) {
		folderName := sess.StartedAt.UTC().Format("2006-01-02") + "-" + slugify(sess.Title)
		if err := s.renameSessionFolder(ctx, sess.GDriveFolderID, folderName); err != nil {
			return err
		}
		return s.replacements.FinishDriveReplacement(sess.ID, generation)
	}

	recordings, err := s.replacements.PendingDriveReplacementRecordings(sess.ID)
	if err != nil {
		return fmt.Errorf("list replacement recordings for session %s: %w", sess.ID, err)
	}
	if len(recordings) == 0 {
		recordings = []string{splicedRecording}
	}
	files, folderName, err := replacementFiles(&sess, recordings)
	if err != nil {
		return fmt.Errorf("build replacements for session %s: %w", sess.ID, err)
	}
	existing, err := s.ListFolderFiles(ctx, sess.GDriveFolderID)
	if err != nil {
		return fmt.Errorf("list replacements for session %s: %w", sess.ID, err)
	}
	oldByName := make(map[string][]*drive.File, len(files))
	for _, file := range existing {
		for i := range files {
			if file.Name == files[i].Name {
				oldByName[file.Name] = append(oldByName[file.Name], file)
			}
		}
	}
	for i := range files {
		file := &files[i]
		rows := driveReplacementsNamed(replacements, file.Name)
		if len(rows) == 0 {
			oldFiles := oldByName[file.Name]
			if err := s.replacements.StartDriveReplacement(sess.ID, file.Name, generation, driveFileIDs(oldFiles)); err != nil {
				return err
			}
			replacements, err = s.replacements.DriveReplacements(sess.ID, generation)
			if err != nil {
				return err
			}
			rows = driveReplacementsNamed(replacements, file.Name)
		}
		if !driveReplacementIsUploading(rows) {
			continue
		}
		key := replacementUploadKey(sess.ID, file.Name, rows)
		newFile, err := replacementFileForKey(existing, file.Name, key)
		if err != nil {
			return err
		}
		if file.SHA256 == "" {
			file.SHA256 = contentHash(file)
		}
		newID := ""
		if newFile == nil {
			file.AppProperties = map[string]string{replacementKeyProperty: key}
			newID, err = s.uploadFileID(ctx, sess.GDriveFolderID, file)
			if err != nil {
				return err
			}
		} else {
			newID = newFile.Id
		}
		if err := s.verifyFile(ctx, newID, file); err != nil {
			return fmt.Errorf("verify replacement %s: %w", file.Name, err)
		}
		if err := s.replacements.MarkDriveReplacementUploaded(sess.ID, file.Name, generation, newID); err != nil {
			return err
		}
		replacements, err = s.replacements.DriveReplacements(sess.ID, generation)
		if err != nil {
			return err
		}
	}

	replacements, err = s.replacements.DriveReplacements(sess.ID, generation)
	if err != nil {
		return err
	}
	if !hasReplacementForEveryFile(replacements, files) {
		return fmt.Errorf("replace session files %s: replacements are incomplete", sess.ID)
	}
	for _, replacement := range replacements {
		switch replacement.State {
		case storage.DriveReplacementDeleted:
			continue
		case storage.DriveReplacementUploaded:
			currentGeneration, err := s.replacements.DeletionGeneration(sess.ID)
			if err != nil {
				return fmt.Errorf("read deletion generation for session %s: %w", sess.ID, err)
			}
			if currentGeneration != generation {
				return fmt.Errorf("replace session files %s: deletion generation changed from %d to %d", sess.ID, generation, currentGeneration)
			}
			if replacement.OldID != "" {
				err := s.service.Files.Delete(replacement.OldID).SupportsAllDrives(true).Context(ctx).Do()
				if err != nil && !IsRemoteDeletedError(err) {
					return fmt.Errorf("delete old %s: %w", replacement.Name, err)
				}
			}
			if err := s.replacements.MarkDriveReplacementDeleted(sess.ID, replacement.Name, replacement.OldID, generation); err != nil {
				return err
			}
		default:
			return fmt.Errorf("replace session files %s: replacement %s has state %q", sess.ID, replacement.Name, replacement.State)
		}
	}
	if err := s.renameSessionFolder(ctx, sess.GDriveFolderID, folderName); err != nil {
		return err
	}
	if err := s.replacements.FinishDriveReplacement(sess.ID, generation); err != nil {
		return err
	}
	return nil
}

func replacementFiles(sess *storage.Session, recordings []string) ([]SyncFile, string, error) {
	if len(recordings) == 0 {
		return nil, "", fmt.Errorf("spliced recording is required")
	}
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
		files = append(files, SyncFile{Name: "audio-" + filepath.Base(recording), MimeType: mimeType, LocalPath: recording, ContentType: mimeType, SHA256: hash})
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

func replacementFileForKey(files []*drive.File, name, key string) (*drive.File, error) {
	var found *drive.File
	for _, file := range files {
		if file.Name != name || file.AppProperties[replacementKeyProperty] != key {
			continue
		}
		if found == nil || file.ModifiedTime > found.ModifiedTime {
			found = file
		}
	}
	return found, nil
}
