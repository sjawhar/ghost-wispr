package gc

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// namingDrive answers as Drive does when it cannot say: its error names the
// folder it was asked about. The folder check fails from call failFolderFrom
// on (1 is the first); failMatch makes the recordings check fail.
type namingDrive struct {
	failFolderFrom int
	failMatch      bool
	calls          *int
}

func (d namingDrive) FolderOnDrive(_ context.Context, id string) (bool, error) {
	*d.calls++
	if d.failFolderFrom > 0 && *d.calls >= d.failFolderFrom {
		return false, fmt.Errorf("get folder %s: googleapi: Error 503: Backend Error, backendError", id)
	}
	return true, nil
}

func (d namingDrive) RecordingsMatch(_ context.Context, folderID string, _ map[string]string) (bool, error) {
	if d.failMatch {
		return false, fmt.Errorf("list folder %s: googleapi: Error 503: Backend Error, backendError", folderID)
	}
	return true, nil
}

// TestAGCHoldNamesNoDriveFolder: GET /api/status shows the GC hold until the
// next pass, which can be an hour, also once its session is deleted. Drive's
// error names the session's Drive folder id; the hold keeps why Drive could
// not say, and leaves the folder id out.
func TestAGCHoldNamesNoDriveFolder(t *testing.T) {
	const folder = "1AbCdEfGhIjKlMnOpQrStUvWxYz012345"
	for _, tc := range []struct {
		name  string
		drive namingDrive
	}{
		{"the folder check fails", namingDrive{failFolderFrom: 1, calls: new(int)}},
		{"the recordings check fails, then the folder check", namingDrive{failFolderFrom: 2, failMatch: true, calls: new(int)}},
		{"the recordings check fails", namingDrive{failMatch: true, calls: new(int)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newStore(t)
			dir := t.TempDir()
			const id = "20260901100000"
			endWithRecording(t, store, dir, id, daysAgo(60), 1024)
			if err := store.UpdateSummaryAtGeneration(id, "Standup", "A summary.", storage.SummaryCompleted, "default", "{}", "", 0); err != nil {
				t.Fatal(err)
			}
			syncTo(t, store, id, storage.SyncStateSynced, folder, 0, "")
			collector := New(store, Config{MaxAgeDays: 30, AudioDir: dir, Drive: tc.drive})
			if _, err := collector.Run(); err != nil {
				t.Fatal(err)
			}
			held := collector.Held()
			if held == nil {
				t.Fatal("control: the pass did not hold back")
			}
			if !strings.Contains(held.Error(), "Backend Error") {
				t.Errorf("hold %q does not say why Drive could not say", held)
			}
			if strings.Contains(held.Error(), folder) {
				t.Errorf("hold %q names the session's Drive folder", held)
			}
		})
	}
}
