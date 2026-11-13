package erase

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
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

// TestBeginLeavesTheFileForFinishOrLeftovers fails if Begin leaves the file
// at its name, or overwrites it before Finish runs, or if a file Begin hid
// and no Finish reached (the app stopped) is not finished by Leftovers.
func TestBeginLeavesTheFileForFinishOrLeftovers(t *testing.T) {
	for _, stopped := range []bool{false, true} {
		dir := t.TempDir()
		path := filepath.Join(dir, "recording.mp3")
		held := holdOpen(t, path, []byte(secret))
		pending, err := Begin(path)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the file still has its name after Begin: %v", err)
		}
		got := make([]byte, len(secret))
		if _, err := held.ReadAt(got, 0); err != nil || string(got) != secret {
			t.Fatalf("Begin overwrote the file before Finish: %q (%v)", got, err)
		}
		if stopped {
			err = Leftovers(dir)
		} else {
			err = pending.Finish()
		}
		if err != nil {
			t.Fatalf("finish (stopped %v): %v", stopped, err)
		}
		assertZeroed(t, held, len(secret))
		if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
			t.Errorf("left in the directory (stopped %v): %v (%v)", stopped, entries, err)
		}
	}
}

// TestBeginFlushesTheRenameBeforeItReturns fails if Begin returns before the
// directory holding the file is flushed after the rename to its hidden name:
// DELETE /api/sessions/{id} answers once Begin returns, and a power loss
// could then bring the recording back under its own name, which no row names
// and the startup cleanup does not erase. If the flush fails, Begin's error
// wraps ErrLeftover (the file is under its hidden name, for Finish or
// Leftovers) and Finish still erases it.
func TestBeginFlushesTheRenameBeforeItReturns(t *testing.T) {
	for _, flushErr := range []error{nil, errors.New("input/output error")} {
		dir := t.TempDir()
		path := filepath.Join(dir, "recording.mp3")
		held := holdOpen(t, path, []byte(secret))
		var flushed []string
		original := syncDirectory
		syncDirectory = func(d string) error {
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a flush of %s ran with the file still at its name: %v", d, err)
			}
			flushed = append(flushed, d)
			if len(flushed) == 1 {
				return flushErr
			}
			return nil
		}
		pending, err := Begin(path)
		beganFlushes := len(flushed)
		syncDirectory = original
		if flushErr == nil && err != nil {
			t.Fatalf("Begin: %v", err)
		}
		if flushErr != nil && !errors.Is(err, ErrLeftover) {
			t.Errorf("Begin with a failed flush = %v, want an error wrapping ErrLeftover", err)
		}
		if beganFlushes != 1 || flushed[0] != dir {
			t.Fatalf("Begin flushed %v, want the directory %s once", flushed, dir)
		}
		if err := pending.Finish(); err != nil {
			t.Fatalf("Finish: %v", err)
		}
		assertZeroed(t, held, len(secret))
	}
}

// TestFinishAfterOrphansFinishedTheFileReportsNothing fails if Finish
// reports a file left unerased when Orphans (the sync sweep, in the same
// directory) zeroed and removed it first: that is a false warning, since the
// file is erased.
func TestFinishAfterOrphansFinishedTheFileReportsNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "recording.mp3")
	held := holdOpen(t, path, []byte(secret))
	pending, err := Begin(path)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := Orphans(dir); err != nil {
		t.Fatalf("Orphans: %v", err)
	}
	assertZeroed(t, held, len(secret))
	if err := pending.Finish(); err != nil {
		t.Errorf("Finish after Orphans erased the file = %v, want nil", err)
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

// TestLeftoversDoesNotFollowASymlink fails if a .erasing-* name that is a
// symbolic link (never one Remove or Replace makes) has the file it points to
// overwritten: anyone able to write the directory could aim it at any file.
func TestLeftoversDoesNotFollowASymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "elsewhere.txt")
	if err := os.WriteFile(target, []byte("not ours to erase"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, hiddenPrefix+"planted")); err != nil {
		t.Fatal(err)
	}
	for name, finish := range map[string]func(string) error{"Leftovers": Leftovers, "Orphans": Orphans} {
		if err := finish(dir); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got, err := os.ReadFile(target); err != nil || string(got) != "not ours to erase" {
			t.Errorf("%s changed the file a planted symlink points to: %q (%v)", name, got, err)
		}
	}
}

// TestOrphansLeavesASecondNameAlone fails if Orphans, which runs while the
// app runs, unlinks the hidden second name Replace keeps before its rename:
// the rename would then free the replaced recording without overwriting it.
// An orphan no other name links is overwritten and removed.
func TestOrphansLeavesASecondNameAlone(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "recording.mp3")
	if err := os.WriteFile(live, []byte("the live recording"), 0o600); err != nil {
		t.Fatal(err)
	}
	second := filepath.Join(dir, hiddenPrefix+"second-name")
	if err := os.Link(live, second); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(dir, hiddenPrefix+"orphan")
	held := holdOpen(t, orphan, []byte(secret))

	if err := Orphans(dir); err != nil {
		t.Fatalf("Orphans: %v", err)
	}
	if _, err := os.Stat(second); err != nil {
		t.Errorf("Orphans unlinked the second name of a recording not yet replaced: %v", err)
	}
	if got, err := os.ReadFile(live); err != nil || string(got) != "the live recording" {
		t.Errorf("live recording after Orphans = %q (%v), want it whole", got, err)
	}
	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the orphan is still there: %v", err)
	}
	assertZeroed(t, held, len(secret))
}

func TestReservationChoosesTheHiddenPathBeforeTheRename(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recording.mp3")
	if err := os.WriteFile(path, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	reservation, err := Reserve(path)
	if err != nil {
		t.Fatal(err)
	}
	if reservation.OriginalPath() != path {
		t.Errorf("reserved original path = %q, want %q", reservation.OriginalPath(), path)
	}
	if filepath.Dir(reservation.HiddenPath()) != filepath.Dir(path) || !strings.HasPrefix(filepath.Base(reservation.HiddenPath()), hiddenPrefix) {
		t.Errorf("reserved hidden path = %q, want an erasing name beside %q", reservation.HiddenPath(), path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("reservation hid %q before Begin: %v", path, err)
	}
	pending, err := reservation.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(reservation.HiddenPath()); err != nil {
		t.Fatalf("Begin did not use the reserved hidden path %q: %v", reservation.HiddenPath(), err)
	}
	if err := pending.Restore(); err != nil {
		t.Fatal(err)
	}
}

func TestLeftoverFinishersSkipReservedHiddenPaths(t *testing.T) {
	for name, finish := range map[string]func(string, func(string) bool) error{
		"Leftovers": LeftoversExcept,
		"Orphans":   OrphansExcept,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			hidden := filepath.Join(dir, hiddenPrefix+"reserved")
			held := holdOpen(t, hidden, []byte(secret))
			if err := finish(dir, func(path string) bool { return path == hidden }); err != nil {
				t.Fatalf("%s with reserved path: %v", name, err)
			}
			if got, err := os.ReadFile(hidden); err != nil || string(got) != secret {
				t.Fatalf("%s changed reserved hidden path to %q (%v)", name, got, err)
			}
			if err := finish(dir, nil); err != nil {
				t.Fatalf("%s without reservation: %v", name, err)
			}
			assertZeroed(t, held, len(secret))
		})
	}
}
