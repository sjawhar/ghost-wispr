package gdrive

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestDownloadRecordingWritesTheDriveFileAtomically fails if a download does
// not put Drive's recording at the destination by a rename, or rewrites the
// local recording it replaces first. A download serves a segment deletion's
// splice, and a segment deletion zeroes no whole audio file (Sami, 2026-11-12:
// "Do NOT zero entire files if people just want to delete certain segments",
// dispatch://INFRA-2961/ask/172349fa-0019-488c-9d3b-9d6b7af26a81).
func TestDownloadRecordingWritesTheDriveFileAtomically(t *testing.T) {
	wire, syncer := newWireDrive(t)
	const folder = "session-folder"
	wire.add(folder, "audio-20261111120000.mp3", "audio/mpeg", []byte("recording from Drive"), nil)
	destination := filepath.Join(t.TempDir(), "20261111120000.mp3")
	old := []byte("old local recording")
	if err := os.WriteFile(destination, old, 0o600); err != nil {
		t.Fatalf("write destination: %v", err)
	}
	held, err := os.Open(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()

	if err := syncer.DownloadRecording(context.Background(), folder, "audio-20261111120000.mp3", destination); err != nil {
		t.Fatalf("DownloadRecording: %v", err)
	}
	contents, err := os.ReadFile(destination)
	if err != nil {
		t.Fatalf("read downloaded recording: %v", err)
	}
	if !bytes.Equal(contents, []byte("recording from Drive")) {
		t.Fatalf("downloaded recording = %q", contents)
	}
	replaced := make([]byte, len(old))
	if _, err := held.ReadAt(replaced, 0); err != nil {
		t.Fatalf("read the replaced recording: %v", err)
	}
	if !bytes.Equal(replaced, old) {
		t.Errorf("the download rewrote the recording it replaced: it holds %q", replaced)
	}
}

func TestDownloadRecordingLeavesExistingFileWhenDriveFileIsMissing(t *testing.T) {
	_, syncer := newWireDrive(t)
	destination := filepath.Join(t.TempDir(), "recording.mp3")
	original := []byte("old local recording")
	if err := os.WriteFile(destination, original, 0o600); err != nil {
		t.Fatalf("write destination: %v", err)
	}

	if err := syncer.DownloadRecording(context.Background(), "session-folder", "audio-missing.mp3", destination); err == nil {
		t.Fatal("DownloadRecording succeeded for a missing Drive file")
	}
	contents, err := os.ReadFile(destination)
	if err != nil {
		t.Fatalf("read destination: %v", err)
	}
	if !bytes.Equal(contents, original) {
		t.Fatalf("missing Drive file changed destination: got %q want %q", contents, original)
	}
}
