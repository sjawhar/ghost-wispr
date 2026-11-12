package erase

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

const secret = "original audio bytes of a deleted segment"

// holdOpen writes content to path and returns a descriptor on that file,
// which keeps reading its inode after the name is gone.
func holdOpen(t *testing.T, path string, content []byte) *os.File {
	t.Helper()
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// assertZeroed fails unless the file f holds is as long as before and all
// zeros: the bytes the file system frees with it no longer hold its content.
func assertZeroed(t *testing.T, f *os.File, size int) {
	t.Helper()
	got, err := io.ReadAll(io.NewSectionReader(f, 0, int64(size)+1))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != size {
		t.Fatalf("the old file holds %d bytes, want its %d bytes overwritten in place", len(got), size)
	}
	if !bytes.Equal(got, make([]byte, size)) {
		t.Errorf("the old file still holds %q", got)
	}
}

func TestRemoveZeroesTheFileBeforeItIsFreed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recording.mp3")
	held := holdOpen(t, path, []byte(secret))
	if err := Remove(path); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the file is still at its name: %v", err)
	}
	assertZeroed(t, held, len(secret))
	if entries, err := os.ReadDir(filepath.Dir(path)); err != nil || len(entries) != 0 {
		t.Errorf("left in the directory: %v (%v)", entries, err)
	}
}

func TestRemoveOfAMissingFileChangesNothing(t *testing.T) {
	if err := Remove(filepath.Join(t.TempDir(), "missing.mp3")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Remove of a missing file = %v, want not-exist", err)
	}
}

func TestReplaceZeroesTheFileItReplaces(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "recording.mp3")
	held := holdOpen(t, path, []byte(secret))
	spliced := filepath.Join(dir, ".splice-1.mp3")
	if err := os.WriteFile(spliced, []byte("spliced audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Replace(spliced, path); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "spliced audio" {
		t.Fatalf("recording after Replace = %q (%v), want the spliced audio", got, err)
	}
	assertZeroed(t, held, len(secret))
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 1 {
		t.Errorf("left in the directory: %v (%v), want only the recording", entries, err)
	}
}

func TestReplaceOfAFailedRenameKeepsTheRecording(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "recording.mp3")
	if err := os.WriteFile(path, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Replace(filepath.Join(dir, "missing.mp3"), path); err == nil || errors.Is(err, ErrLeftover) {
		t.Fatalf("Replace from a missing file = %v, want a plain failure", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != secret {
		t.Errorf("recording after a failed Replace = %q (%v), want it whole", got, err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 1 {
		t.Errorf("left in the directory: %v (%v), want only the recording", entries, err)
	}
}

// TestLeftoversZeroesOnlyWhatNothingElseLinks fails if Leftovers leaves an
// erasure the app stopped part way, or zeroes a file another name still
// links: the second name Replace keeps before it renames, whose recording
// is still the live one.
func TestLeftoversZeroesOnlyWhatNothingElseLinks(t *testing.T) {
	dir := t.TempDir()
	cutOff := filepath.Join(dir, hiddenPrefix+"cut-off")
	held := holdOpen(t, cutOff, []byte(secret))
	live := filepath.Join(dir, "recording.mp3")
	if err := os.WriteFile(live, []byte("the live recording"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(live, filepath.Join(dir, hiddenPrefix+"second-name")); err != nil {
		t.Fatal(err)
	}

	if err := Leftovers(dir); err != nil {
		t.Fatalf("Leftovers: %v", err)
	}
	assertZeroed(t, held, len(secret))
	if got, err := os.ReadFile(live); err != nil || string(got) != "the live recording" {
		t.Errorf("live recording after Leftovers = %q (%v), want it whole", got, err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 1 {
		t.Errorf("left in the directory: %v (%v), want only the live recording", entries, err)
	}
}
