package gdrive

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// TestReplacementSpliceWaitWarnsAfterFiveMinutes proves that a recently
// started splice waits quietly, but a stuck splice emits an actionable warning.
func TestReplacementSpliceWaitWarnsAfterFiveMinutes(t *testing.T) {
	for _, tc := range []struct {
		name string
		age  time.Duration
		warn bool
	}{
		{name: "fresh", age: time.Minute},
		{name: "stuck", age: 6 * time.Minute, warn: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newReplacementFixture(t)
			fixture.queue(t, storage.RefinementCompleted, storage.SummaryCompleted)
			deletedAt := time.Now().UTC().Add(-tc.age).Format(time.RFC3339Nano)
			if _, err := fixture.store.DB().Exec(`UPDATE deleted_segments SET splice_complete = 0, deleted_at = ? WHERE session_id = ?`, deletedAt, fixture.id); err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			o := NewOrchestrator(fixture.syncer, fixture.store, slog.New(slog.NewTextHandler(&logs, nil)))

			if err := o.SyncSession(context.Background(), fixture.id); err != nil {
				t.Fatalf("SyncSession: %v", err)
			}
			output := logs.String()
			if tc.warn {
				if !strings.Contains(output, "level=WARN") || !strings.Contains(output, "warning_after=5m0s") {
					t.Errorf("stuck splice log = %q, want warning with its five-minute threshold", output)
				}
			} else if strings.Contains(output, "level=WARN") {
				t.Errorf("fresh splice log = %q, want no warning before five minutes", output)
			}
			if fixture.wire.uploads != 3 {
				t.Errorf("splice wait uploaded %d files, want only the fixture's initial three", fixture.wire.uploads)
			}
		})
	}
}
