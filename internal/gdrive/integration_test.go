//go:build integration

package gdrive

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	driveapi "google.golang.org/api/drive/v3"

	"github.com/sjawhar/ghost-wispr/internal/googleauth"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

func TestSyncToRealDrive(t *testing.T) {
	credPath := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")
	folderID := os.Getenv("GDRIVE_TEST_FOLDER_ID")
	if credPath == "" || folderID == "" {
		t.Skip("GOOGLE_APPLICATION_CREDENTIALS and GDRIVE_TEST_FOLDER_ID required")
	}

	ctx := context.Background()
	credentials, err := googleauth.Detect(ctx, credPath, driveapi.DriveFileScope)
	if err != nil {
		t.Fatalf("detect credentials: %v", err)
	}
	syncer, err := NewSyncer(ctx, credentials, folderID)
	if err != nil {
		t.Fatalf("create syncer: %v", err)
	}

	started := time.Now().UTC()
	ended := started.Add(5 * time.Minute)

	sess := SyncSession{
		ID:            "integration-test-" + started.Format("20060102-150405"),
		Title:         "Integration Test Session",
		StartedAt:     started,
		EndedAt:       &ended,
		Summary:       "This is an integration test summary.",
		SummaryPreset: "default",
	}
	segments := []transcribe.Segment{
		{Speaker: 0, Text: "Integration test segment", StartTime: 0.0, EndTime: 1.0, Timestamp: started},
	}

	tmpAudio := t.TempDir() + "/test.mp3"
	if err := os.WriteFile(tmpAudio, []byte("fake-audio-data"), 0o644); err != nil {
		t.Fatal(err)
	}

	files, folderName, err := BuildSyncFiles(&sess, segments, tmpAudio)
	if err != nil {
		t.Fatalf("build sync files: %v", err)
	}

	driveFolderID, err := syncer.Upload(ctx, folderName, files)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}

	t.Logf("Created Drive folder: %s (ID: %s)", folderName, driveFolderID)
}

// TestRealDriveTellsTheRootFromALostOne asks Drive, and changes nothing on
// it, whether the folder the app syncs into (GDRIVE_TEST_FOLDER_ID, a shared
// drive's id on the office Pi) reads, through the app's drive.file scope. A
// made-up id (GDRIVE_TEST_FOLDER_ID with its last character changed, which
// Drive answered 404 for on 2026-11-11) stands for a session folder deleted
// from that root, which is gone (REMOTE_DELETED), and for a root Drive does
// not have, in which the app's access to Drive may be lost and a restore
// that finds nothing fails. It also checks that the session folders the app
// made in the shared drive list without corpora, as a restore lists them;
// with none there yet, that subtest skips as UNKNOWN.
func TestRealDriveTellsTheRootFromALostOne(t *testing.T) {
	credPath := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")
	folderID := os.Getenv("GDRIVE_TEST_FOLDER_ID")
	if credPath == "" || folderID == "" {
		t.Skip("GOOGLE_APPLICATION_CREDENTIALS and GDRIVE_TEST_FOLDER_ID required")
	}
	ctx := context.Background()
	credentials, err := googleauth.Detect(ctx, credPath, driveapi.DriveFileScope)
	if err != nil {
		t.Fatalf("detect credentials: %v", err)
	}
	syncer := func(root string) *Syncer {
		s, err := NewSyncer(ctx, credentials, root)
		if err != nil {
			t.Fatalf("create syncer: %v", err)
		}
		return s
	}
	madeUp := folderID[:len(folderID)-1] + "X"
	if strings.HasSuffix(folderID, "X") {
		madeUp = folderID[:len(folderID)-1] + "Y"
	}

	root := syncer(folderID)
	if err := root.rootReadable(ctx); err != nil {
		t.Errorf("root %s: %v, want it to read", folderID, err)
	}
	if on, err := root.FolderOnDrive(ctx, madeUp); on || err != nil {
		t.Errorf("session folder %s, which root %s does not have: FolderOnDrive = %v, %v; want false, no error (gone)", madeUp, folderID, on, err)
	}
	listed, err := root.ListSessionFolders(ctx)
	if err != nil {
		t.Errorf("list session folders in %s: %v", folderID, err)
	}
	// A restore, and every list of a session folder, lists without corpora.
	// Drive documents that such a list holds the shared drives' files the
	// app has accessed; this checks it against the shared drive's own list.
	t.Run("session folders list as the shared drive lists them", func(t *testing.T) {
		var inDrive []string
		q := fmt.Sprintf("'%s' in parents and mimeType = 'application/vnd.google-apps.folder' and trashed = false", folderID)
		err := root.service.Files.List().Corpora("drive").DriveId(folderID).Q(q).
			SupportsAllDrives(true).IncludeItemsFromAllDrives(true).
			Fields("nextPageToken, files(id)").Context(ctx).
			Pages(ctx, func(page *driveapi.FileList) error {
				for _, f := range page.Files {
					inDrive = append(inDrive, f.Id)
				}
				return nil
			})
		if err != nil {
			t.Skipf("UNKNOWN: %s does not list as a shared drive (%v), so there is nothing to compare", folderID, err)
		}
		if len(inDrive) == 0 {
			t.Skipf("UNKNOWN: shared drive %s holds no folder the app made, so the listing cannot be compared", folderID)
		}
		seen := map[string]bool{}
		for _, f := range listed {
			seen[f.Id] = true
		}
		for _, id := range inDrive {
			if !seen[id] {
				t.Errorf("folder %s is in shared drive %s, and missing from its session folders listed without corpora", id, folderID)
			}
		}
		t.Logf("compared %d folders", len(inDrive))
	})

	lost := syncer(madeUp)
	if err := lost.rootReadable(ctx); err == nil {
		t.Errorf("root %s, which Drive does not have: it reads", madeUp)
	}
	if on, err := lost.FolderOnDrive(ctx, madeUp); on || err == nil || !strings.Contains(err.Error(), "access to Drive may be lost") {
		t.Errorf("a session folder in root %s, which Drive does not have: FolderOnDrive = %v, %v; want access to Drive lost", madeUp, on, err)
	}
	if _, err := lost.ListSessionFolders(ctx); err == nil {
		t.Errorf("list session folders in %s, which Drive does not have: no error", madeUp)
	}
}
