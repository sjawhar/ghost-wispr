package main

import (
	"bytes"
	"context"
	"os"
	"testing"

	"google.golang.org/api/drive/v3"
)

type r11ObservedDownloadDrive struct {
	files      []*drive.File
	downloads  map[string][]byte
	onDownload func(destination string)
}

func (d *r11ObservedDownloadDrive) ListFolderFiles(context.Context, string) ([]*drive.File, error) {
	return d.files, nil
}

func (d *r11ObservedDownloadDrive) DownloadRecording(_ context.Context, _ string, name, destination string) error {
	if err := os.WriteFile(destination, d.downloads[name], 0o600); err != nil {
		return err
	}
	if d.onDownload != nil {
		d.onDownload(destination)
	}
	return nil
}

// TestR11DeletionRestoreStagesBeforeInstall fails if the Delete text and audio
// restore writes a Drive recording to its real path before its hash is checked.
func TestR11DeletionRestoreStagesBeforeInstall(t *testing.T) {
	fixture := newR9MergedCutFixture(t)
	postCut := []byte("recording C after cut")
	files := append([]*drive.File(nil), fixture.drive.files...)
	files[1] = &drive.File{Name: files[1].Name, MimeType: files[1].MimeType, Sha256Checksum: fixture.deletedC.SHA256After}
	var during []byte
	source := &r11ObservedDownloadDrive{
		files: files,
		downloads: map[string][]byte{
			files[1].Name: postCut,
		},
		onDownload: func(string) { during, _ = os.ReadFile(fixture.audioC) },
	}
	session, err := fixture.store.GetSession("merged")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restoreDeletionRecordings(context.Background(), fixture.store, &session, fixture.dir, source, ""); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(during, postCut) {
		t.Fatal("Delete text and audio exposed the downloaded recording at its real path before verification")
	}
	got, err := os.ReadFile(fixture.audioC)
	if err != nil || !bytes.Equal(got, postCut) {
		t.Fatalf("installed recording = %q, %v; want verified Drive bytes", got, err)
	}
}
