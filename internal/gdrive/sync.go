package gdrive

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

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

func BuildSyncFiles(sess *SyncSession, segments []transcribe.Segment, audioPath string) ([]SyncFile, string, error) {
	date := sess.StartedAt.UTC().Format("2006-01-02")
	slug := slugify(sess.Title)
	folderName := date + "-" + slug

	var files []SyncFile

	if strings.TrimSpace(sess.Summary) != "" {
		summaryMD := RenderSummaryMarkdown(sess)
		files = append(files, SyncFile{
			Name:        "summary.md",
			MimeType:    "application/vnd.google-apps.document",
			Content:     []byte(summaryMD),
			ContentType: "text/plain",
		})
	}

	transcriptMD := RenderTranscriptMarkdown(sess, segments)
	files = append(files, SyncFile{
		Name:        "transcript.md",
		MimeType:    "application/vnd.google-apps.document",
		Content:     []byte(transcriptMD),
		ContentType: "text/plain",
	})

	if audioPath != "" {
		if _, err := os.Stat(audioPath); err == nil {
			ext := filepath.Ext(audioPath)
			mimeType := "audio/mpeg"
			if ext == ".wav" {
				mimeType = "audio/wav"
			}
			files = append(files, SyncFile{
				Name:        "audio" + ext,
				MimeType:    mimeType,
				LocalPath:   audioPath,
				ContentType: mimeType,
			})
		}
	}

	return files, folderName, nil
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

	for _, f := range files {
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
				return folder.Id, fmt.Errorf("open %s: %w", f.LocalPath, err)
			}
			file = openedFile
			reader = file
			info, err := file.Stat()
			if err != nil {
				_ = file.Close()
				return folder.Id, fmt.Errorf("stat %s: %w", f.LocalPath, err)
			}
			fileSize = info.Size()
			if shouldUseResumableUpload(fileSize) {
				useResumable = true
			}
		default:
			continue
		}

		createCall := s.service.Files.Create(&drive.File{
			Name:     f.Name,
			MimeType: f.MimeType,
			Parents:  []string{folder.Id},
		}).SupportsAllDrives(true).Context(ctx).Fields("id")

		if useResumable {
			s.logger.Info("using resumable upload", "operation", "upload", "file_name", f.Name, "file_size_bytes", fileSize)
			createCall = createCall.ResumableMedia(ctx, file, fileSize, f.ContentType)
		} else {
			createCall = createCall.Media(reader, googleapi.ContentType(f.ContentType))
		}

		_, err := createCall.Do()
		if file != nil {
			_ = file.Close()
		}
		if err != nil {
			return folder.Id, fmt.Errorf("upload %s: %w", f.Name, err)
		}
	}

	return folder.Id, nil
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
