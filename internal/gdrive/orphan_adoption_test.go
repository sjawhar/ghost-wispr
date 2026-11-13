package gdrive

import (
	"context"
	"testing"
)

// TestSyncAdoptsALegacyFolderByTranscriptIDAfterARetitle fails if a session
// with an unrecorded pre-upgrade folder makes a second folder after its title
// changed. A folder's transcript identifies its session; date and title do
// not, because another meeting can have the same name.
func TestSyncAdoptsALegacyFolderByTranscriptIDAfterARetitle(t *testing.T) {
	store := openStore(t)
	const id = "legacy-orphan"
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
		t.Fatalf("create legacy folder: %v", err)
	}
	cutOffBeforeFolderRecorded(t, store, id)
	if err := store.UpdateTitle(id, "Retitled meeting"); err != nil {
		t.Fatal(err)
	}
	if err := NewOrchestrator(syncer, store).SyncSession(context.Background(), id); err != nil {
		t.Fatalf("sync: %v", err)
	}
	after, err := store.GetSession(id)
	if err != nil {
		t.Fatal(err)
	}
	if after.GDriveFolderID != orphan {
		t.Fatalf("sync stored folder %q, want legacy folder %q identified by its transcript", after.GDriveFolderID, orphan)
	}
	if folders, err := syncer.ListSessionFolders(context.Background()); err != nil || len(folders) != 1 {
		t.Fatalf("Drive folders after legacy adoption = %+v, %v; want the legacy folder only", folders, err)
	}
	_ = wire
}
