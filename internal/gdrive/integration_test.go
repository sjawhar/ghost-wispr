//go:build integration

package gdrive

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

func TestSyncToRealDrive(t *testing.T) {
	credPath := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")
	folderID := os.Getenv("GDRIVE_TEST_FOLDER_ID")
	if credPath == "" || folderID == "" {
		t.Skip("GOOGLE_APPLICATION_CREDENTIALS and GDRIVE_TEST_FOLDER_ID required")
	}

	ctx := context.Background()
	syncer, err := NewSyncer(ctx, credPath, folderID)
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

	files, folderName, err := BuildSyncFiles(sess, segments, tmpAudio)
	if err != nil {
		t.Fatalf("build sync files: %v", err)
	}

	driveFolderID, err := syncer.Upload(ctx, folderName, files)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}

	t.Logf("Created Drive folder: %s (ID: %s)", folderName, driveFolderID)
}
