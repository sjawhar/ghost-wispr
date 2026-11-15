package main

import (
	"context"
	"os"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// TestR10RejectsDownloadWhoseBytesDifferFromDriveChecksum fails if a Drive
// listing says a cut recording has its latest bytes but the downloaded bytes do
// not; the downloaded pre-cut copy must be removed rather than used for A's
// deletion.
func TestR10RejectsDownloadWhoseBytesDifferFromDriveChecksum(t *testing.T) {
	fixture := newR9MergedCutFixture(t)
	fixture.drive.files[1].Sha256Checksum = fixture.deletedC.SHA256After
	runner := &segmentDeleteRunner{
		store:    fixture.store,
		audioDir: fixture.dir,
		ensureRecordings: func(ctx context.Context, session storage.Session) (deletionRecordingSet, error) {
			return restoreDeletionRecordings(ctx, fixture.store, &session, fixture.dir, fixture.drive, "")
		},
		duration: func(context.Context, string) (float64, error) { return 10, nil },
		splice:   fakeSplice,
	}
	if err := runner.DeleteRefinedAudio(context.Background(), "merged", fixture.segmentA, fixture.revision); err == nil {
		t.Fatal("delete accepted a downloaded recording whose bytes were pre-cut")
	}
	if _, err := os.Stat(fixture.audioC); !os.IsNotExist(err) {
		t.Fatalf("checksum-mismatched download remains at C: %v", err)
	}
}
