package gdrive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2/google"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"

	"github.com/sjawhar/ghost-wispr/internal/logging"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

type Syncer struct {
	service  *drive.Service
	folderID string
	logger   *slog.Logger
	mu       sync.Mutex
}

const resumableUploadThresholdBytes int64 = 5 * 1024 * 1024

type SyncFile struct {
	Name        string
	MimeType    string
	Content     []byte
	LocalPath   string
	ContentType string
}

func NewSyncer(ctx context.Context, credPath, folderID string, logger ...*slog.Logger) (*Syncer, error) {
	creds, err := os.ReadFile(credPath)
	if err != nil {
		return nil, fmt.Errorf("read credentials: %w", err)
	}

	config, err := google.CredentialsFromJSONWithTypeAndParams(ctx, creds, google.ServiceAccount, google.CredentialsParams{Scopes: []string{drive.DriveFileScope}})
	if err != nil {
		return nil, fmt.Errorf("parse credentials: %w", err)
	}

	svc, err := drive.NewService(ctx, option.WithCredentials(config))
	if err != nil {
		return nil, fmt.Errorf("create drive service: %w", err)
	}

	l := logging.WithModule(slog.Default(), "gdrive")
	if len(logger) > 0 && logger[0] != nil {
		l = logging.WithModule(logger[0], "gdrive")
	}

	return &Syncer{
		service:  svc,
		folderID: folderID,
		logger:   l,
	}, nil
}

// renderDocuments returns the documents a sync uploads for s: its summary
// ("" when it has none) and its transcript, from the canonical transcript if
// it has one, else from its segments.
func renderDocuments(s *SyncSession, segments []transcribe.Segment) (summary, transcript string) {
	if strings.TrimSpace(s.Summary) != "" {
		summary = RenderSummaryMarkdown(s)
	}
	if strings.TrimSpace(s.CanonicalTranscript) != "" {
		return summary, RenderCanonicalTranscriptMarkdown(s)
	}
	return summary, RenderTranscriptMarkdown(s, segments)
}

func BuildSyncFiles(sess *SyncSession, segments []transcribe.Segment, audioPath string) ([]SyncFile, string, error) {
	date := sess.StartedAt.UTC().Format("2006-01-02")
	slug := slugify(sess.Title)
	folderName := date + "-" + slug

	var files []SyncFile

	summaryMD, transcriptMD := renderDocuments(sess, segments)
	if summaryMD != "" {
		files = append(files, SyncFile{
			Name:        "summary.md",
			MimeType:    "application/vnd.google-apps.document",
			Content:     []byte(summaryMD),
			ContentType: "text/plain",
		})
	}
	files = append(files, SyncFile{
		Name:        "transcript.md",
		MimeType:    "application/vnd.google-apps.document",
		Content:     []byte(transcriptMD),
		ContentType: "text/plain",
	})

	// Each recording the audio path names (a merged session's names several)
	// is uploaded as "audio-" and its own file name: a name in the folder
	// identifies one recording, whatever the audio path names before or
	// after, so a re-sync that finds a name in the folder (UploadInto) can
	// only find the recording it uploaded before. One missing from disk is
	// left out here; notUploaded names it, and the sync does not count as
	// done without it.
	for _, recording := range recordingEntries(audioPath) {
		if _, err := os.Stat(recording); err != nil {
			continue
		}
		ext := filepath.Ext(recording)
		mimeType := "audio/mpeg"
		if ext == ".wav" {
			mimeType = "audio/wav"
		}
		name := "audio-" + filepath.Base(recording)
		files = append(files, SyncFile{
			Name:        name,
			MimeType:    mimeType,
			LocalPath:   recording,
			ContentType: mimeType,
		})
	}

	return files, folderName, nil
}

// recordingEntries returns the recordings an audio path names: a merged
// session's names several, separated by commas.
func recordingEntries(audioPath string) []string {
	var recordings []string
	for _, entry := range strings.Split(audioPath, ",") {
		if entry = strings.TrimSpace(entry); entry != "" {
			recordings = append(recordings, entry)
		}
	}
	return recordings
}

// notUploaded returns the recordings an audio path names that files, the
// files built for an upload, leave out: they were missing from disk when the
// list was built (their disk not mounted, say).
func notUploaded(audioPath string, files []SyncFile) []string {
	uploaded := make(map[string]bool, len(files))
	for i := range files {
		uploaded[files[i].LocalPath] = true
	}
	var missing []string
	for _, recording := range recordingEntries(audioPath) {
		if !uploaded[recording] {
			missing = append(missing, recording)
		}
	}
	return missing
}

func (s *Syncer) Upload(ctx context.Context, folderName string, files []SyncFile) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	folder, err := s.service.Files.Create(&drive.File{
		Name:     folderName,
		MimeType: "application/vnd.google-apps.folder",
		Parents:  []string{s.folderID},
	}).SupportsAllDrives(true).Context(ctx).Fields("id").Do()
	if err != nil {
		return "", fmt.Errorf("create folder %s: %w", folderName, err)
	}

	for i := range files {
		if err := s.uploadFile(ctx, folder.Id, &files[i]); err != nil {
			return folder.Id, err
		}
	}

	return folder.Id, nil
}

// driveFile is a file in a session's Drive folder.
type driveFile struct {
	ID, Name string
	// ContentHash is the hash of a document (summary, transcript) this app
	// uploaded, kept in the file's appProperties; "" for audio, and for a
	// document uploaded before hashes were kept.
	ContentHash  string
	ModifiedTime time.Time
}

// newestFile returns the most recently modified of files (the first listed,
// on a tie). A folder holds one copy of each document, which a re-sync
// updates in place; one an earlier version of this app left with two copies
// is read, and updated, through the newest only.
func newestFile(files []driveFile) driveFile {
	newest := files[0]
	for _, f := range files[1:] {
		if f.ModifiedTime.After(newest.ModifiedTime) {
			newest = f
		}
	}
	return newest
}

// toDriveFile is f as planUpload and restore read it.
func toDriveFile(f *drive.File) driveFile {
	modified, _ := time.Parse(time.RFC3339Nano, f.ModifiedTime)
	return driveFile{ID: f.Id, Name: f.Name, ContentHash: f.AppProperties[contentHashProperty], ModifiedTime: modified}
}

// contentHashProperty is the appProperties key a document's hash is kept in.
const contentHashProperty = "ghostWisprSHA256"

// contentHash returns the hash of a document's content, and "" for a
// recording.
func contentHash(f *SyncFile) string {
	if f.LocalPath != "" || f.Content == nil {
		return ""
	}
	sum := sha256.Sum256(f.Content)
	return hex.EncodeToString(sum[:])
}

// docUpdate is a document a re-sync updates in place: the file on Drive, and
// its new content.
type docUpdate struct {
	ID   string
	File SyncFile
}

// planUpload returns, for a re-sync into a session folder holding existing,
// the files to create and the documents to update in place. Nothing on Drive
// is ever deleted. A recording is created unless the folder has a file of
// its name, which (named after the recording) can only be that recording. A
// document is created if the folder has none of its name; otherwise its
// newest copy stays if it holds the same content (the same hash), and is
// updated in place if not: the same file, id, comments and links, with
// Drive's revision history keeping the earlier text and any edit made there.
func planUpload(existing []driveFile, files []SyncFile) (create []SyncFile, update []docUpdate) {
	byName := map[string][]driveFile{}
	for _, f := range existing {
		byName[f.Name] = append(byName[f.Name], f)
	}
	for i := range files {
		f := &files[i]
		have := byName[f.Name]
		switch {
		case len(have) == 0:
			create = append(create, *f)
		case f.LocalPath != "":
			// The recording is there.
		default:
			if doc := newestFile(have); doc.ContentHash != contentHash(f) {
				update = append(update, docUpdate{ID: doc.ID, File: *f})
			}
		}
	}
	return create, update
}

// UploadInto brings an existing session folder up to date (see planUpload):
// a session synced before, or whose first sync created its folder and then
// failed, keeps its one folder when it is synced again.
func (s *Syncer) UploadInto(ctx context.Context, folderID string, files []SyncFile) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var existing []driveFile
	query := fmt.Sprintf("'%s' in parents and trashed = false", strings.ReplaceAll(folderID, "'", `\'`))
	err := s.service.Files.List().Q(query).SupportsAllDrives(true).IncludeItemsFromAllDrives(true).
		Fields("nextPageToken, files(id, name, appProperties, modifiedTime)").Context(ctx).
		Pages(ctx, func(page *drive.FileList) error {
			for _, f := range page.Files {
				existing = append(existing, toDriveFile(f))
			}
			return nil
		})
	if err != nil {
		return fmt.Errorf("list folder %s: %w", folderID, err)
	}
	create, update := planUpload(existing, files)
	for i := range create {
		if err := s.uploadFile(ctx, folderID, &create[i]); err != nil {
			return err
		}
	}
	for i := range update {
		if err := s.updateDocument(ctx, &update[i]); err != nil {
			return err
		}
	}
	return nil
}

// updateDocument replaces a document's content in place, and its content
// hash: the file, its id, comments and links stay, and Drive's revision
// history keeps the earlier content.
func (s *Syncer) updateDocument(ctx context.Context, u *docUpdate) error {
	meta := &drive.File{AppProperties: map[string]string{contentHashProperty: contentHash(&u.File)}}
	_, err := s.service.Files.Update(u.ID, meta).
		Media(bytes.NewReader(u.File.Content), googleapi.ContentType(u.File.ContentType)).
		SupportsAllDrives(true).Context(ctx).Fields("id").Do()
	if err != nil {
		return fmt.Errorf("update %s: %w", u.File.Name, err)
	}
	return nil
}

// uploadFile uploads one file into a Drive folder.
func (s *Syncer) uploadFile(ctx context.Context, folderID string, f *SyncFile) error {
	var reader io.Reader
	var file *os.File
	var fileSize int64
	var useResumable bool

	switch {
	case f.Content != nil:
		reader = bytes.NewReader(f.Content)
	case f.LocalPath != "":
		openedFile, err := os.Open(f.LocalPath)
		if err != nil {
			return fmt.Errorf("open %s: %w", f.LocalPath, err)
		}
		file = openedFile
		reader = file
		info, err := file.Stat()
		if err != nil {
			_ = file.Close()
			return fmt.Errorf("stat %s: %w", f.LocalPath, err)
		}
		fileSize = info.Size()
		if shouldUseResumableUpload(fileSize) {
			useResumable = true
		}
	default:
		return nil
	}

	meta := &drive.File{
		Name:     f.Name,
		MimeType: f.MimeType,
		Parents:  []string{folderID},
	}
	if hash := contentHash(f); hash != "" {
		meta.AppProperties = map[string]string{contentHashProperty: hash}
	}
	createCall := s.service.Files.Create(meta).SupportsAllDrives(true).Context(ctx).Fields("id")

	if useResumable {
		s.logger.Info("using resumable upload", "operation", "upload", "file_name", f.Name, "file_size_bytes", fileSize)
		createCall = createCall.Media(file, googleapi.ContentType(f.ContentType))
	} else {
		createCall = createCall.Media(reader, googleapi.ContentType(f.ContentType))
	}

	_, err := createCall.Do()
	if file != nil {
		_ = file.Close()
	}
	if err != nil {
		return fmt.Errorf("upload %s: %w", f.Name, err)
	}
	return nil
}

func shouldUseResumableUpload(fileSize int64) bool {
	return fileSize > resumableUploadThresholdBytes
}

func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return "untitled"
	}

	var b strings.Builder
	prevHyphen := false
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			prevHyphen = false
		} else if r == ' ' && !prevHyphen && b.Len() > 0 {
			b.WriteRune('-')
			prevHyphen = true
		}
	}

	result := strings.TrimRight(b.String(), "-")
	if result == "" {
		return "untitled"
	}
	return result
}
