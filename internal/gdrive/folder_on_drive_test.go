package gdrive

import (
	"context"
	"testing"
)

// Syncer.FolderOnDrive over the wire: Drive has the folder, has it in the
// trash, does not have it (404, while the folder the app syncs into reads), or
// cannot say: Drive does not answer, or a 404 for the folder comes with a
// 404 for the folder the app syncs into too (the app's access to Drive is
// lost, which says nothing about the session). GC marks a session for good
// only on a false with no error.
func TestSyncerFolderOnDriveOverTheWire(t *testing.T) {
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
			wire.deleteByHand("root")
		}, false, true},
		{"Drive not answering", func(wire *wireDrive, _ string) { wire.unavailable = true }, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire, syncer := newWireDrive(t)
			folder := wire.add("root", "2026-11-06 Standup", "application/vnd.google-apps.folder", nil, nil)
			tc.setUp(wire, folder)

			on, err := syncer.FolderOnDrive(context.Background(), folder)
			if on != tc.want || (err != nil) != tc.wantErr {
				t.Errorf("FolderOnDrive = %v, %v; want %v, error %v", on, err, tc.want, tc.wantErr)
			}
		})
	}
}
