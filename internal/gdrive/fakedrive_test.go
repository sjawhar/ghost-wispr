package gdrive

import (
	"bytes"
	"context"
	"crypto/md5"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

// fakeDriveFile is a file in fakeDrive.
type fakeDriveFile struct {
	driveFile
	content []byte
}

// fakeDrive is Google Drive as session folders of files, which a re-sync
// fills through planUpload, as the Syncer does: the same decisions, with
// Drive's storage faked. failAfter > 0 makes uploads fail once that many files
// went up.
type fakeDrive struct {
	mu        sync.Mutex
	folders   map[string][]fakeDriveFile
	nextID    int
	failAfter int
	uploaded  int
	changes   int
	// onUpload, when set, runs as an upload starts, outside the lock: what
	// happens elsewhere while a sync uploads.
	onUpload func()
	// onFolderCreated runs after Upload creates a folder and before it puts
	// the first file into it.
	onFolderCreated func()
}

func newFakeDrive() *fakeDrive { return &fakeDrive{folders: map[string][]fakeDriveFile{}} }

var errUploadCut = errors.New("upload cut off")

func (d *fakeDrive) put(folderID string, f *SyncFile) error {
	if d.failAfter > 0 && d.uploaded >= d.failAfter {
		return fmt.Errorf("upload %s: %w", f.Name, errUploadCut)
	}
	content := f.Content
	if f.LocalPath != "" {
		var err error
		if content, err = os.ReadFile(f.LocalPath); err != nil {
			return fmt.Errorf("open %s: %w", f.LocalPath, err)
		}
	}
	d.nextID++
	d.uploaded++
	d.folders[folderID] = append(d.folders[folderID], fakeDriveFile{
		driveFile: driveFile{ID: fmt.Sprintf("file-%d", d.nextID), Name: f.Name, ContentHash: contentHash(f), ModifiedTime: d.tick()},
		content:   content,
	})
	return nil
}

// tick returns the time of the next change to a file.
func (d *fakeDrive) tick() time.Time {
	d.changes++
	return time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(d.changes) * time.Minute)
}

// updateInPlace replaces a document's content in place, as the Syncer does.
func (d *fakeDrive) updateInPlace(folderID string, u *docUpdate) error {
	if d.failAfter > 0 && d.uploaded >= d.failAfter {
		return fmt.Errorf("update %s: %w", u.File.Name, errUploadCut)
	}
	for i := range d.folders[folderID] {
		if f := &d.folders[folderID][i]; f.ID == u.ID {
			d.uploaded++
			f.content, f.ContentHash, f.ModifiedTime = u.File.Content, contentHash(&u.File), d.tick()
			return nil
		}
	}
	return fmt.Errorf("update %s: no file %s", u.File.Name, u.ID)
}

func (d *fakeDrive) Upload(ctx context.Context, folderName string, files []SyncFile) (string, error) {
	return d.upload(ctx, folderName, files, nil)
}

func (d *fakeDrive) UploadWithFolderRecorded(ctx context.Context, folderName string, files []SyncFile, record func(folderID string) error) (string, error) {
	return d.upload(ctx, folderName, files, record)
}

func (d *fakeDrive) upload(_ context.Context, _ string, files []SyncFile, record func(folderID string) error) (string, error) {
	if d.onUpload != nil {
		d.onUpload()
	}
	d.mu.Lock()
	d.nextID++
	folderID := fmt.Sprintf("folder-%d", d.nextID)
	d.folders[folderID] = nil
	d.mu.Unlock()
	if record != nil {
		if err := record(folderID); err != nil {
			return folderID, err
		}
	}
	if d.onFolderCreated != nil {
		d.onFolderCreated()
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for i := range files {
		if err := d.put(folderID, &files[i]); err != nil {
			return folderID, err
		}
	}
	return folderID, nil
}

func (d *fakeDrive) UploadInto(_ context.Context, folderID string, files []SyncFile) error {
	if d.onUpload != nil {
		d.onUpload()
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	existing := make([]driveFile, 0, len(d.folders[folderID]))
	for _, f := range d.folders[folderID] {
		existing = append(existing, f.driveFile)
	}
	create, update := planUpload(existing, files)
	for i := range create {
		if err := d.put(folderID, &create[i]); err != nil {
			return err
		}
	}
	for i := range update {
		if err := d.updateInPlace(folderID, &update[i]); err != nil {
			return err
		}
	}
	return nil
}


// FolderOnDrive reports whether the fake holds folder folderID.
func (d *fakeDrive) FolderOnDrive(_ context.Context, folderID string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.folders[folderID]
	return ok, nil
}

func (d *fakeDrive) RecordingsMatch(_ context.Context, folderID string, checksums map[string]string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for name, checksum := range checksums {
		matched := false
		for _, f := range d.folders[folderID] {
			if f.Name != name {
				continue
			}
			sum := md5.Sum(f.content)
			if fmt.Sprintf("%x", sum) == checksum {
				matched = true
				break
			}
		}
		if !matched {
			return false, nil
		}
	}
	return true, nil
}

// has reports whether folder folderID holds a file with this content.
func (d *fakeDrive) has(folderID string, content []byte) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, f := range d.folders[folderID] {
		if bytes.Equal(f.content, content) {
			return true
		}
	}
	return false
}

// files returns the names of the files in folder folderID, and the content
// of each.
func (d *fakeDrive) files(folderID string) map[string][]byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	files := map[string][]byte{}
	for _, f := range d.folders[folderID] {
		files[f.Name] = f.content
	}
	return files
}
