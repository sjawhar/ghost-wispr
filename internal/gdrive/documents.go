package gdrive

import (
	"context"
	"errors"
	"fmt"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// documentsStore is what a sync reads and writes to bring the Drive folders
// of merged sources up to date after "Delete text" (storage.DeleteSegmentText)
// changed their documents.
type documentsStore interface {
	DriveDocumentsOwed(sessionID string) ([]string, error)
	ClearDriveDocumentsPending(sessionID string, unchanged func(storage.Session) bool) (bool, error)
}

// emptiedSummary is summary.md for a session folder whose session now has no
// summary: a segment deletion cleared it, and the summary made again came
// back empty or failed. It updates the folder's summary.md in place, so its
// current text does not keep the deleted words, and creates none in a folder
// that has none.
func emptiedSummary() SyncFile {
	return SyncFile{Name: "summary.md", MimeType: "application/vnd.google-apps.document", Content: []byte{}, ContentType: "text/plain", UpdateOnly: true}
}

// withEmptiedSummary returns files, the files a sync into an existing folder
// uploads, with emptiedSummary when they hold no summary.md.
func withEmptiedSummary(files []SyncFile) []SyncFile {
	for i := range files {
		if files[i].Name == "summary.md" {
			return files
		}
	}
	return append(files, emptiedSummary())
}

// refreshOwedDocuments updates in place, in the Drive folder of each merged
// source of session sessionID (or of sessionID itself, when it is one) that
// "Delete text" left owing its documents, the transcript and summary as the
// source has them now. A folder in the trash stays owed, and the sweep comes
// back once it is restored; a folder gone for good owes nothing.
func (o *Orchestrator) refreshOwedDocuments(ctx context.Context, sessionID string) error {
	docs, ok := o.store.(documentsStore)
	if !ok {
		return nil
	}
	owed, err := docs.DriveDocumentsOwed(sessionID)
	if err != nil {
		return err
	}
	var failures []error
	for _, id := range owed {
		if err := o.refreshDocuments(ctx, docs, id); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (o *Orchestrator) refreshDocuments(ctx context.Context, docs documentsStore, id string) error {
	sess, err := o.store.GetSession(id)
	if err != nil {
		return fmt.Errorf("get merged session %s: %w", id, err)
	}
	segments, err := o.store.GetSegments(id)
	if err != nil {
		return fmt.Errorf("get segments %s: %w", id, err)
	}
	syncSess := toSyncSession(&sess)
	files, folderName, _, err := buildSyncFiles(&syncSess, segments, "")
	if err != nil {
		return fmt.Errorf("build the documents of merged session %s: %w", id, err)
	}
	summary, transcript := renderDocuments(&syncSess, segments)
	err = o.uploader.UploadInto(ctx, sess.GDriveFolderID, withEmptiedSummary(files))
	if err == nil {
		if syncer, ok := o.uploader.(*Syncer); ok {
			err = syncer.renameSessionFolder(ctx, sess.GDriveFolderID, folderName)
		}
	}
	switch {
	case err == nil:
	case errors.Is(err, ErrFolderTrashed):
		// It comes back, old documents and all, if restored: still owed.
		return fmt.Errorf("the Drive folder of merged session %s is in the trash; its documents are updated once it is restored: %w", id, err)
	case IsRemoteDeletedError(err):
		o.logger.Info("gdrive merged source folder is gone; its documents are owed nowhere", "operation", "refresh_documents", "session_id", id, "drive_folder_id", sess.GDriveFolderID)
	default:
		return fmt.Errorf("update the documents of merged session %s in Drive folder %s: %w", id, sess.GDriveFolderID, err)
	}
	cleared, err := docs.ClearDriveDocumentsPending(id, func(now storage.Session) bool {
		nowSync := toSyncSession(&now)
		nowSummary, nowTranscript := renderDocuments(&nowSync, segments)
		return now.GDriveFolderID == sess.GDriveFolderID &&
			now.Title == sess.Title &&
			nowSummary == summary &&
			nowTranscript == transcript
	})
	if err != nil {
		return err
	}
	if !cleared {
		return fmt.Errorf("merged session %s changed while its Drive documents were updated; the next sync updates them again", id)
	}
	o.logger.Info("gdrive merged source documents updated in place", "operation", "refresh_documents", "session_id", id, "drive_folder_id", sess.GDriveFolderID)
	return nil
}
