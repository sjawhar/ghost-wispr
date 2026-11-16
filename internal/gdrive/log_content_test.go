package gdrive

import (
	"context"
	"strings"
	"testing"
)

// TestAFailedFolderCreateDoesNotNameTheFolder: a session's Drive folder is
// named for its date and title, and a title names the conversation's topic.
// The error of a folder Drive does not create goes to the app's log
// ("gdrive sync failed") and to the session's sync error: it names the root
// folder and why, not the folder's name.
func TestAFailedFolderCreateDoesNotNameTheFolder(t *testing.T) {
	const folderName = "2026-11-16-purple-elephant-seventeen-budget"
	_, syncer := newWireDrive(t)
	// A root Drive does not have: the create is refused.
	syncer.folderID = "missing-root"
	_, err := syncer.Upload(context.Background(), folderName, nil)
	if err == nil {
		t.Fatal("control: Drive created a folder in a root it does not have")
	}
	if !strings.Contains(err.Error(), "missing-root") {
		t.Errorf("error %q does not say which root refused the folder", err)
	}
	for _, word := range []string{"purple", "elephant", "seventeen", "budget"} {
		if strings.Contains(strings.ToLower(err.Error()), word) {
			t.Errorf("error %q names the folder's topic (%q)", err, word)
		}
	}
}
