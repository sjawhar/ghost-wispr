package gdrive

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestDownloadRecordingWritesTheDriveFileAtomically(t *testing.T) {
	wire, syncer := newWireDrive(t)
	const folder = "session-folder"
	wire.add(folder, "audio-20261111120000.mp3", "audio/mpeg", []byte("recording from Drive"), nil)
	destination := filepath.Join(t.TempDir(), "20261111120000.mp3")
	if err := os.WriteFile(destination, []byte("old local recording"), 0o600); err != nil {
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
	// The recording replaced was overwritten before the file system freed it.
	old := make([]byte, len("old local recording"))
	if _, err := held.ReadAt(old, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(old, make([]byte, len(old))) {
		t.Errorf("the replaced recording still holds %q", old)
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
