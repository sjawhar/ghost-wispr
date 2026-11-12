package storage

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// TestDeleteSessionReturnsTheAudioPathItDeleted fails if DeleteSession does
// not report the audio_path the row had when it went: the session delete
// route erases those recordings, not ones it read before the delete.
func TestDeleteSessionReturnsTheAudioPathItDeleted(t *testing.T) {
	store := newTestSQLiteStore(t)
	started := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	if err := store.CreateSession("gone", started); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("gone", started.Add(time.Minute), "data/audio/gone.mp3"); err != nil {
		t.Fatal(err)
	}
	audioPath, err := store.DeleteSession("gone")
	if err != nil {
		t.Fatal(err)
	}
	if audioPath != "data/audio/gone.mp3" {
		t.Errorf("DeleteSession returned %q, want the deleted row's audio_path", audioPath)
	}
}

// TestRecordingDirectoriesListsWhereARelativeEntryPoints fails if the
// directory a relative audio path entry points to (from the working
// directory, as the recorder writes filepath.Join(audio_dir, name)) is not
// listed, as an absolute path: after audio_dir changes with the recording
// left there, a segment deletion splices it there (recording.Resolve), and
// the startup cleanup must reach the copies a stop leaves beside it.
func TestRecordingDirectoriesListsWhereARelativeEntryPoints(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	store := newTestSQLiteStore(t)
	started := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	if err := store.CreateSession("left", started); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("left", started.Add(time.Minute), filepath.Join("old-audio", "left.mp3")); err != nil {
		t.Fatal(err)
	}
	dirs, err := store.RecordingDirectories()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "old-audio"); !slices.Contains(dirs, want) {
		t.Errorf("recording directories %v, want %s, where the relative entry points", dirs, want)
	}
}

// TestEraseDirectoriesAreForgottenOnlyWhereTheyWereCleaned fails if an
// erase directory is forgotten when the startup cleanup could not have
// reached it: when it is not there, or is on another file system than the
// erase was (a disk not mounted, its mount point showing the file system
// below). Kept, it is cleaned at a later start. It also fails if one the
// cleanup reached is not forgotten, or a directory that is not there is
// recorded at all.
func TestEraseDirectoriesAreForgottenOnlyWhereTheyWereCleaned(t *testing.T) {
	store := newTestSQLiteStore(t)
	listed := func(dir string) bool {
		t.Helper()
		dirs, err := store.RecordingDirectories()
		if err != nil {
			t.Fatal(err)
		}
		return slices.Contains(dirs, dir)
	}
	record := func(dir string) {
		t.Helper()
		if err := store.RecordEraseDirectory(dir); err != nil {
			t.Fatal(err)
		}
		if !listed(dir) {
			t.Fatalf("control: the recorded erase directory %s is not listed", dir)
		}
	}
	forget := func(dir string) {
		t.Helper()
		if err := store.ForgetEraseDirectories([]string{dir}); err != nil {
			t.Fatal(err)
		}
	}

	cleaned := t.TempDir()
	record(cleaned)
	forget(cleaned)
	if listed(cleaned) {
		t.Error("an erase directory the cleanup reached is still listed")
	}

	absent := filepath.Join(t.TempDir(), "unplugged")
	if err := os.Mkdir(absent, 0o700); err != nil {
		t.Fatal(err)
	}
	record(absent)
	if err := os.Remove(absent); err != nil {
		t.Fatal(err)
	}
	forget(absent)
	if !listed(absent) {
		t.Error("an erase directory that was not there to clean was forgotten")
	}

	unmounted := t.TempDir()
	record(unmounted)
	if _, err := store.DB().Exec(`UPDATE erase_directories SET device = device + 1 WHERE path = ?`, unmounted); err != nil {
		t.Fatal(err)
	}
	forget(unmounted)
	if !listed(unmounted) {
		t.Error("an erase directory on another file system than the erase was forgotten")
	}

	missing := filepath.Join(t.TempDir(), "never-there")
	if err := store.RecordEraseDirectory(missing); err != nil {
		t.Fatal(err)
	}
	if listed(missing) {
		t.Error("a directory that is not there was recorded")
	}
}

// eraseDirectoryRows counts the erase_directories rows of dir.
func eraseDirectoryRows(t *testing.T, store *SQLiteStore, dir string) int {
	t.Helper()
	var n int
	if err := store.DB().QueryRow(`SELECT count(*) FROM erase_directories WHERE path = ?`, dir).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestAnEraseDirectoryOwedOnAnotherFileSystemIsKept fails if recording an
// erase in a directory overwrites the row of an earlier erase there on
// another file system (a disk not mounted now, its mount point showing the
// file system below): the startup cleanup then cleans the directory on the
// file system it shows and forgets it, and the cut-off erase on the disk is
// never finished once the disk is back.
func TestAnEraseDirectoryOwedOnAnotherFileSystemIsKept(t *testing.T) {
	store := newTestSQLiteStore(t)
	dir := t.TempDir()
	if err := store.RecordEraseDirectory(dir); err != nil {
		t.Fatal(err)
	}
	// That erase was on a disk, now not mounted.
	if _, err := store.DB().Exec(`UPDATE erase_directories SET device = device + 1 WHERE path = ?`, dir); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordEraseDirectory(dir); err != nil {
		t.Fatal(err)
	}
	if n := eraseDirectoryRows(t, store, dir); n != 2 {
		t.Fatalf("erase_directories holds %d rows of the directory, want one for each file system", n)
	}
	if err := store.ForgetEraseDirectories([]string{dir}); err != nil {
		t.Fatal(err)
	}
	if n := eraseDirectoryRows(t, store, dir); n != 1 {
		t.Errorf("after the cleanup of the file system mounted now, %d rows are left, want the disk's one", n)
	}
}

// TestEraseDirectoriesOfAnEarlierSchemaAreKeptAndKeyedByFileSystem fails if
// opening a database whose erase_directories is keyed on the path alone (as
// an earlier build of this unit made it) loses a row, or leaves the table
// keyed on the path, so a second file system's erase in the directory
// overwrites the first.
func TestEraseDirectoriesOfAnEarlierSchemaAreKeptAndKeyedByFileSystem(t *testing.T) {
	path := filepath.Join(t.TempDir(), "erase.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for _, stmt := range []string{
		`DROP TABLE erase_directories`,
		`CREATE TABLE erase_directories (path TEXT PRIMARY KEY, device INTEGER NOT NULL)`,
	} {
		if _, err := store.DB().Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.DB().Exec(`INSERT INTO erase_directories(path, device) VALUES(?, -7)`, dir); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if n := eraseDirectoryRows(t, store, dir); n != 1 {
		t.Fatalf("after the upgrade, %d rows of the directory, want the earlier one kept", n)
	}
	if err := store.RecordEraseDirectory(dir); err != nil {
		t.Fatal(err)
	}
	if n := eraseDirectoryRows(t, store, dir); n != 2 {
		t.Errorf("after an erase on this file system, %d rows of the directory, want the earlier one and this one", n)
	}
}
