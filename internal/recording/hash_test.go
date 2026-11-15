package recording

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestSHA256HashesTheFile fails if SHA256 does not return the hex SHA-256 of
// the file's bytes, or hides that the file cannot be read.
func TestSHA256HashesTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "20261106100000.mp3")
	if err := os.WriteFile(path, []byte("audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	// printf audio | sha256sum
	const want = "6ed8919ce20490a5e3ad8630a4fab69475297abd07db73918dd5f36fcfaeb11b"
	if got, err := SHA256(path); err != nil || got != want {
		t.Fatalf("SHA256 = %q, %v; want %q", got, err, want)
	}
	if _, err := SHA256(filepath.Join(t.TempDir(), "missing.mp3")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("SHA256 of a missing file = %v, want os.ErrNotExist", err)
	}
}
