package gdrive

import (
	"context"
	"fmt"
	"os"
	"sync"

	"golang.org/x/oauth2/google"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
)

type Syncer struct {
	service  *drive.Service
	folderID string
	fileIDs  map[string]string
	mu       sync.Mutex
}

func NewSyncer(ctx context.Context, credPath, folderID string) (*Syncer, error) {
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

	return &Syncer{
		service:  svc,
		folderID: folderID,
		fileIDs:  make(map[string]string),
	}, nil
}

func (s *Syncer) Sync(localPath, date string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	f, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", localPath, err)
	}
	defer func() { _ = f.Close() }()

	name := fmt.Sprintf("ghost-wispr-%s", date)

	if fileID, ok := s.fileIDs[date]; ok {
		_, err = s.service.Files.Update(fileID, &drive.File{}).Media(f).Do()
		if err != nil {
			return fmt.Errorf("drive update: %w", err)
		}
		return nil
	}

	doc, err := s.service.Files.Create(&drive.File{
		Name:     name,
		MimeType: "application/vnd.google-apps.document",
		Parents:  []string{s.folderID},
	}).Media(f).Do()
	if err != nil {
		return fmt.Errorf("drive create: %w", err)
	}

	s.fileIDs[date] = doc.Id
	return nil
}
