package gdrive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/auth"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"

	"github.com/sjawhar/ghost-wispr/internal/logging"
	recordingname "github.com/sjawhar/ghost-wispr/internal/recording"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

type Syncer struct {
	service      *drive.Service
	folderID     string
	logger       *slog.Logger
	mu           sync.Mutex
	replacements replacementStore
}

const resumableUploadThresholdBytes int64 = 5 * 1024 * 1024

type SyncFile struct {
	Name          string
	MimeType      string
	Content       []byte
	LocalPath     string
	ContentType   string
	SHA256        string
	AppProperties map[string]string
	// UpdateOnly is a document a re-sync updates in place if the folder has
	// one, and creates in no folder: the summary a segment deletion emptied
	// (emptiedSummary), whose earlier copy would keep the deleted words.
	UpdateOnly bool
}

func NewSyncer(ctx context.Context, credentials *auth.Credentials, folderID string, logger ...*slog.Logger) (*Syncer, error) {
	l := logging.WithModule(slog.Default(), "gdrive")
	if len(logger) > 0 && logger[0] != nil {
		l = logging.WithModule(logger[0], "gdrive")
	}
	return newSyncerWithLogger(ctx, credentials, folderID, l)
}

func newSyncerWithLogger(ctx context.Context, credentials *auth.Credentials, folderID string, logger *slog.Logger, options ...option.ClientOption) (*Syncer, error) {
	if credentials == nil {
		return nil, fmt.Errorf("google Drive credentials are required")
	}
	options = append([]option.ClientOption{option.WithAuthCredentials(credentials)}, options...)
	svc, err := drive.NewService(ctx, options...)
	if err != nil {
		return nil, fmt.Errorf("create drive service: %w", err)
	}

	return &Syncer{
		service:  svc,
		folderID: folderID,
		logger:   logger,
	}, nil
}

// BuildSyncFiles returns the files a sync uploads, or an error when a
// recording could not be read. The orchestrator uses buildSyncFiles so it can
// upload the readable files while it leaves a session pending for that
// recording.
func BuildSyncFiles(sess *SyncSession, segments []transcribe.Segment, audioPath string) ([]SyncFile, string, error) {
	files, folderName, unreadable, err := buildSyncFiles(sess, segments, audioPath)
	if err != nil {
		return nil, "", err
	}
	if len(unreadable) != 0 {
		return nil, "", fmt.Errorf("read recordings: %s", strings.Join(unreadable, ", "))
	}
	return files, folderName, nil
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

func buildSyncFiles(sess *SyncSession, segments []transcribe.Segment, audioPath string) ([]SyncFile, string, []string, error) {
	date := sess.StartedAt.UTC().Format("2006-01-02")
	slug := slugify(sess.Title)
	folderName := date + "-" + slug

	var files []SyncFile
	var unreadable []string

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
		hash, err := fileSHA256(recording)
		if err != nil {
			unreadable = append(unreadable, recording)
			continue
		}
		ext := filepath.Ext(recording)
		mimeType := "audio/mpeg"
		if ext == ".wav" {
			mimeType = "audio/wav"
		}
		name := recordingname.FileName(recording)
		files = append(files, SyncFile{
			Name:        name,
			MimeType:    mimeType,
			LocalPath:   recording,
			ContentType: mimeType,
			SHA256:      hash,
		})
	}

	return files, folderName, unreadable, nil
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

// resolvedAudioPath returns audioPath with each recording it names replaced
// by where recording.Resolve finds it: as written, else its name in
// audioDir.
func resolvedAudioPath(audioPath, audioDir string) string {
	entries := recordingEntries(audioPath)
	for i, entry := range entries {
		entries[i] = recordingname.Resolve(entry, audioDir)
	}
	return strings.Join(entries, ",")
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

// Upload creates a session folder and puts files into it. Callers that need
// the folder durable before the first upload use UploadWithFolderRecorded.
func (s *Syncer) Upload(ctx context.Context, folderName string, files []SyncFile) (string, error) {
	return s.upload(ctx, folderName, files, nil)
}

// UploadWithFolderRecorded creates a session folder, calls record with its id
// before the first file upload, then uploads files into it.
func (s *Syncer) UploadWithFolderRecorded(ctx context.Context, folderName string, files []SyncFile, record func(folderID string) error) (string, error) {
	return s.upload(ctx, folderName, files, record)
}

func (s *Syncer) upload(ctx context.Context, folderName string, files []SyncFile, record func(folderID string) error) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// A folder made inside a root in the trash would be in the trash too.
	// Only a folder in My Drive can be: a shared drive's root cannot be
	// trashed, and under the drive.file scope files.get answers 404 for it
	// (rootReadable), which skips this check as any error does; the create
	// below then answers for the root.
	if root, err := s.service.Files.Get(s.folderID).SupportsAllDrives(true).Fields("id, trashed").Context(ctx).Do(); err == nil && root.Trashed {
		return "", errRootTrashed(s.folderID)
	}
	folder, err := s.service.Files.Create(&drive.File{
		Name:     folderName,
		MimeType: "application/vnd.google-apps.folder",
		Parents:  []string{s.folderID},
	}).SupportsAllDrives(true).Context(ctx).Fields("id").Do()
	if err != nil {
		return "", fmt.Errorf("create folder %s: %w", folderName, err)
	}
	if record != nil {
		if err := record(folder.Id); err != nil {
			return folder.Id, fmt.Errorf("record created folder %s: %w", folder.Id, err)
		}
	}
	for i := range files {
		if files[i].UpdateOnly {
			continue
		}
		if err := s.uploadFile(ctx, folder.Id, &files[i]); err != nil {
			return folder.Id, err
		}
	}
	// The folder may have gone to the trash during the upload.
	if err := s.checkFolder(ctx, folder.Id); err != nil {
		return folder.Id, err
	}

	return folder.Id, nil
}

// ErrFolderTrashed is the error syncing into a session folder that is in the
// Drive trash returns: deleted in the Drive UI, it still exists until the
// trash empties (after 30 days), so Drive does not answer 404 for it.
var ErrFolderTrashed = errors.New("the session's Drive folder is in the trash")

// ErrDriveDidNotAnswer is wrapped in the error of a retry of a
// REMOTE_DELETED session refused because Drive did not say whether the
// session's folder is back: nothing is wrong with the session, and the retry
// may work once Drive answers.
var ErrDriveDidNotAnswer = errors.New("drive did not answer")

// checkFolder returns an error unless Drive has session folder folderID out
// of the trash: Drive's 404 for a folder gone (wrapped), ErrFolderTrashed
// for one in the trash, or why Drive could not say. Drive answers 404 for a
// file the app cannot read as well as for one that is gone, so a 404 counts
// as the folder gone only while the folder the app syncs into still reads
// (rootReadable): otherwise the app's access to Drive is lost (credentials,
// sharing), which says nothing about the session, and the error is not a
// 404.
func (s *Syncer) checkFolder(ctx context.Context, folderID string) error {
	folder, err := s.service.Files.Get(folderID).SupportsAllDrives(true).Fields("id, trashed, explicitlyTrashed").Context(ctx).Do()
	if err != nil {
		if !IsRemoteDeletedError(err) {
			return fmt.Errorf("get folder %s: %w", folderID, err)
		}
		if _, rootErr := s.rootReadable(ctx); rootErr != nil {
			return fmt.Errorf("get folder %s: not found, and the folder the app syncs into (%s) cannot be read either, so access to Drive may be lost: %v", folderID, s.folderID, rootErr)
		}
		return fmt.Errorf("get folder %s: %w", folderID, err)
	}
	if folder.Trashed && !folder.ExplicitlyTrashed {
		// In the trash with a folder above it, most likely the folder the
		// app syncs into: restoring that brings the session back, so this
		// says nothing about the session.
		return fmt.Errorf("folder %s: %w", folderID, errRootTrashed(s.folderID))
	}
	if folder.Trashed {
		return fmt.Errorf("folder %s: %w", folderID, ErrFolderTrashed)
	}
	return nil
}

// rootReadable returns nil when Drive answers for the folder the app syncs
// into, with whether it is a shared drive's root, and why not otherwise.
// files.get reads a folder in My Drive the app can read (under the drive.file
// scope, one it created). It answers 404 for a shared drive's root, as for an
// id Drive does not have, since the app did not create the root. A list of
// the shared drive's files (corpora "drive")
// tells the two apart: it answers for a shared drive the app is a member of,
// and 404 "Shared drive not found" for an id Drive does not have. A list
// without corpora cannot: it answers 200, with no files, for an id Drive does
// not have too. These three answers for a shared drive's root were measured
// through the app on the office Pi on 2026-11-11. A shared drive the app is
// no longer a member of is taken to answer as an id Drive does not have; that
// was not measured. A shared drive reads through the app's membership, not
// through the files the app made there: another account made a member would
// read the drive and get 404 for the folders the first one made.
func (s *Syncer) rootReadable(ctx context.Context) (sharedDrive bool, err error) {
	_, err = s.service.Files.Get(s.folderID).SupportsAllDrives(true).Fields("id").Context(ctx).Do()
	if err == nil || !IsRemoteDeletedError(err) {
		return false, err
	}
	query := fmt.Sprintf("'%s' in parents and trashed = false", strings.ReplaceAll(s.folderID, "'", `\'`))
	_, listErr := s.service.Files.List().Corpora("drive").DriveId(s.folderID).Q(query).PageSize(1).
		SupportsAllDrives(true).IncludeItemsFromAllDrives(true).
		Fields("files(id)").Context(ctx).Do()
	if listErr != nil {
		return false, fmt.Errorf("%v; nor does Drive list it as a shared drive the app is a member of: %v", err, listErr)
	}
	return true, nil
}

// errRootTrashed is the error for a session folder in the trash with a folder
// above it, or a sync into a root in the trash: not a deleted session, so a
// sync retries and GC holds back until the folder is restored.
func errRootTrashed(root string) error {
	return fmt.Errorf("a folder above it is in the Drive trash (the folder the app syncs into is %s): restore it from the trash", root)
}

// FolderOnDrive reports whether Drive has session folder folderID out of the
// trash, and an error when Drive cannot say (it is unreachable, say). GC asks
// it before it deletes a synced session or removes its audio.
func (s *Syncer) FolderOnDrive(ctx context.Context, folderID string) (bool, error) {
	err := s.checkFolder(ctx, folderID)
	switch {
	case err == nil:
		return true, nil
	case IsRemoteDeletedError(err):
		return false, nil
	default:
		return false, err
	}
}

// RecordingsMatch reports whether folderID has every requested recording
// name with the specified Google Drive md5Checksum. Callers confirm the
// folder itself separately: an empty or inaccessible listing does not prove
// whether the folder was removed, but it never verifies a recording.
func (s *Syncer) RecordingsMatch(ctx context.Context, folderID string, checksums map[string]string) (bool, error) {
	if len(checksums) == 0 {
		return true, nil
	}
	remaining := maps.Clone(checksums)
	query := fmt.Sprintf("'%s' in parents and trashed = false", strings.ReplaceAll(folderID, "'", `\'`))
	err := s.service.Files.List().Q(query).SupportsAllDrives(true).IncludeItemsFromAllDrives(true).
		Fields("nextPageToken, files(name, md5Checksum)").Context(ctx).
		Pages(ctx, func(page *drive.FileList) error {
			for _, file := range page.Files {
				if checksum, wanted := remaining[file.Name]; wanted && file.Md5Checksum == checksum {
					delete(remaining, file.Name)
				}
			}
			return nil
		})
	if err != nil {
		return false, fmt.Errorf("list recordings in folder %s: %w", folderID, err)
	}
	return len(remaining) == 0, nil
}

// driveFile is a file in a session's Drive folder.
type driveFile struct {
	ID, Name string
	// ContentHash is the hash this app uploaded in the file's appProperties;
	// it is empty only for a file uploaded before hashes were kept.
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

// contentHash returns the content hash kept on Drive for f.
func contentHash(f *SyncFile) string {
	if f.SHA256 != "" {
		return f.SHA256
	}
	if f.Content == nil {
		return ""
	}
	sum := sha256.Sum256(f.Content)
	return hex.EncodeToString(sum[:])
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
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
			if !f.UpdateOnly {
				create = append(create, *f)
			}
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
// failed, keeps its one folder when it is synced again. It uploads nothing
// into a folder in the trash or gone, and fails if the folder went to the
// trash during the upload (checkFolder), so such a session is never marked
// synced.
func (s *Syncer) UploadInto(ctx context.Context, folderID string, files []SyncFile) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.checkFolder(ctx, folderID); err != nil {
		return err
	}
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
		return s.folderOr(ctx, folderID, fmt.Errorf("list folder %s: %w", folderID, err))
	}
	create, update := planUpload(existing, files)
	for i := range create {
		if err := s.uploadFile(ctx, folderID, &create[i]); err != nil {
			return s.folderOr(ctx, folderID, err)
		}
	}
	for i := range update {
		if err := s.updateDocument(ctx, &update[i]); err != nil {
			return s.folderOr(ctx, folderID, err)
		}
	}
	return s.checkFolder(ctx, folderID)
}

// folderOr returns err, a failure while syncing into session folder
// folderID, unless it is Drive's 404: then a file (or the folder, or access
// to the folder the app syncs into) went since the sync checked the folder,
// and checkFolder, asked again, says which. With the folder still there, the
// 404 was about a file alone: the error is kept but is no 404, so the sync is
// retried rather than the folder taken for deleted.
func (s *Syncer) folderOr(ctx context.Context, folderID string, err error) error {
	if !IsRemoteDeletedError(err) {
		return err
	}
	if folderErr := s.checkFolder(ctx, folderID); folderErr != nil {
		return folderErr
	}
	return fmt.Errorf("%v (the session's folder %s is still on Drive)", err, folderID)
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
	_, err := s.uploadFileID(ctx, folderID, f)
	return err
}

func (s *Syncer) uploadFileID(ctx context.Context, folderID string, f *SyncFile) (string, error) {
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
			return "", fmt.Errorf("open %s: %w", f.LocalPath, err)
		}
		file = openedFile
		reader = file
		info, err := file.Stat()
		if err != nil {
			_ = file.Close()
			return "", fmt.Errorf("stat %s: %w", f.LocalPath, err)
		}
		fileSize = info.Size()
		if shouldUseResumableUpload(fileSize) {
			useResumable = true
		}
	default:
		return "", fmt.Errorf("upload %s: no content", f.Name)
	}

	meta := &drive.File{
		Name:     f.Name,
		MimeType: f.MimeType,
		Parents:  []string{folderID},
	}
	if hash, properties := contentHash(f), f.AppProperties; hash != "" || len(properties) != 0 {
		meta.AppProperties = make(map[string]string, len(properties)+1)
		for key, value := range properties {
			meta.AppProperties[key] = value
		}
		if hash != "" {
			meta.AppProperties[contentHashProperty] = hash
		}
	}
	createCall := s.service.Files.Create(meta).SupportsAllDrives(true).Context(ctx).Fields("id")

	if useResumable {
		s.logger.Info("using resumable upload", "operation", "upload", "file_name", f.Name, "file_size_bytes", fileSize)
		createCall = createCall.Media(file, googleapi.ContentType(f.ContentType))
	} else {
		createCall = createCall.Media(reader, googleapi.ContentType(f.ContentType))
	}

	created, err := createCall.Do()
	if file != nil {
		_ = file.Close()
	}
	if err != nil {
		return "", fmt.Errorf("upload %s: %w", f.Name, err)
	}
	if created.Id == "" {
		return "", fmt.Errorf("upload %s: Drive returned no file id", f.Name)
	}
	return created.Id, nil
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
