package gdrive

import (
	"context"
	"testing"
)

// Syncer.FolderOnDrive over the wire, syncing into a folder in My Drive and
// into a shared drive's root: Drive has the folder, has it in the trash, does
// not have it (404, while the folder the app syncs into reads), or cannot
// say: Drive does not answer, for the folder or, after a 404 for it, for the
// folder the app syncs into, or a 404 for the folder comes with the folder
// the app syncs into not reading either (the app's access to Drive is lost,
// which says nothing about the session). GC marks a session for good only on
// a false with no error.
func TestSyncerFolderOnDriveOverTheWire(t *testing.T) {
	for _, root := range wireRoots {
		for _, tc := range []struct {
			name    string
			setUp   func(wire *wireDrive, folder string)
			want    bool
			wantErr bool
		}{
			{"on Drive", func(*wireDrive, string) {}, true, false},
			{"in the trash", (*wireDrive).trash, false, false},
			{"deleted for good", (*wireDrive).deleteByHand, false, false},
			{"access to Drive lost", func(wire *wireDrive, folder string) {
				wire.deleteByHand(folder)
				wire.deleteByHand(wire.rootID)
			}, false, true},
			{"Drive not answering", func(wire *wireDrive, _ string) { wire.unavailable = true }, false, true},
			{"the root not answering", func(wire *wireDrive, folder string) {
				wire.deleteByHand(folder)
				wire.onGet = func(id string) {
					if id == wire.rootID {
						wire.unavailable = true
					}
				}
			}, false, true},
		} {
			t.Run(root.name+"/"+tc.name, func(t *testing.T) {
				wire, syncer := root.newWire(t)
				folder := wire.add(wire.rootID, "2026-11-06 Standup", "application/vnd.google-apps.folder", nil, nil)
				tc.setUp(wire, folder)

				on, err := syncer.FolderOnDrive(context.Background(), folder)
				if on != tc.want || (err != nil) != tc.wantErr {
					t.Errorf("FolderOnDrive = %v, %v; want %v, error %v", on, err, tc.want, tc.wantErr)
				}
			})
		}
	}
}

// A 404 for a session folder in a shared drive's root says the folder is
// gone only once Drive lists the shared drive (rootReadable): when that list
// does not answer, Drive cannot say, and FolderOnDrive must return an error,
// not the false with no error that GC marks a session REMOTE_DELETED on.
func TestSyncerFolderOnDriveWhenTheSharedDriveDoesNotAnswer(t *testing.T) {
	wire, syncer := newSharedDriveWire(t)
	folder := wire.add(wire.rootID, "2026-11-06 Standup", "application/vnd.google-apps.folder", nil, nil)
	wire.deleteByHand(folder)
	wire.driveUnavailable = true

	if on, err := syncer.FolderOnDrive(context.Background(), folder); on || err == nil {
		t.Errorf("FolderOnDrive = %v, %v; want false with an error", on, err)
	}
}
