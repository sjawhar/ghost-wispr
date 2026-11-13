package gdrive

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// TestSegmentDeletionKeepsALegacyOrphanOwedUntilDriveDiscovery fails if a
// completed splice clears a deletion before its interrupted first sync can
// discover the unrecorded Drive folder that still holds the deleted words.
func TestSegmentDeletionKeepsALegacyOrphanOwedUntilDriveDiscovery(t *testing.T) {
	store := openStore(t)
	const id = "legacy-delete"
	recording, segments := round2Session(t, store, id, []string{"kept", "deleted"})
	wire, syncer := newWireDrive(t)
	session, err := store.GetSession(id)
	if err != nil {
		t.Fatal(err)
	}
	syncSession := toSyncSession(&session)
	files, folderName, _, err := buildSyncFiles(&syncSession, segments, recording)
	if err != nil {
		t.Fatal(err)
	}
	orphan, err := syncer.Upload(context.Background(), folderName, files)
	if err != nil {
		t.Fatalf("create legacy orphan: %v", err)
	}
	cutOffBeforeFolderRecorded(t, store, id)
	span := storage.DeletedSpan{SessionID: id, AudioPath: recording, Start: 1, End: 2, SHA256Before: "before", SHA256After: "after"}
	if err := store.DeleteSegment(id, segments[1].ID, span); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkSpliceComplete(&span); err != nil {
		t.Fatal(err)
	}
	after, err := store.GetSession(id)
	if err != nil {
		t.Fatal(err)
	}
	if !after.DriveReplacePending {
		t.Fatal("the deletion settled before retry could discover the legacy Drive folder")
	}
	if err := os.WriteFile(recording, []byte("spliced audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET refinement_status = ?, summary_status = ? WHERE id = ?`,
		storage.RefinementCompleted, storage.SummaryCompleted, id); err != nil {
		t.Fatal(err)
	}
	wire.allowDeletes = true
	if err := NewOrchestrator(syncer, store).SyncSession(context.Background(), id); err != nil {
		t.Fatalf("replace legacy orphan: %v", err)
	}
	after, err = store.GetSession(id)
	if err != nil {
		t.Fatal(err)
	}
	if after.DriveReplacePending {
		t.Fatal("the replacement stayed pending after it adopted the legacy orphan")
	}
	for _, copy := range wire.in(orphan)["audio-"+id+".mp3"] {
		if strings.Contains(string(copy.Content), "UNSPLICED") {
			t.Fatalf("the legacy orphan still holds unspliced audio %q", copy.Content)
		}
	}
}
