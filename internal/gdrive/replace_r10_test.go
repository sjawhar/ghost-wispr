package gdrive

import (
	"context"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// TestR10ReplacementRefusesRecordingOutsideLatestCut fails if a Drive
// replacement uploads bytes that are not the latest recorded post-cut hash.
func TestR10ReplacementRefusesRecordingOutsideLatestCut(t *testing.T) {
	fixture := newReplacementFixture(t)
	fixture.queue(t, storage.RefinementCompleted, storage.SummaryCompleted)
	if _, err := fixture.store.DB().Exec(`UPDATE deleted_segments SET sha256_after = 'not-the-recording-hash' WHERE session_id = ?`, fixture.id); err != nil {
		t.Fatal(err)
	}
	fixture.wire.allowDeletes = true
	if err := fixture.orchestrator.SyncSession(context.Background(), fixture.id); err == nil {
		t.Fatal("Drive replacement uploaded a recording outside its latest cut hash")
	}
}
