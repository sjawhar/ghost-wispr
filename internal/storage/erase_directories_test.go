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
	audioPath, err := store.DeleteSession("gone", "")
	if err != nil {
		t.Fatal(err)
	}
	if audioPath != "data/audio/gone.mp3" {
		t.Errorf("DeleteSession returned %q, want the deleted row's audio_path", audioPath)
	}
}

// TestDeleteSessionRecordsWhereItsRecordingsAreErasedFirst fails if
// DeleteSession deletes a session's row without first recording the
// directory of each recording it named, found where recording.Resolve finds
// it (as written, else in the audio directory), or deletes the row when it
// cannot record one: an erase of the recording a stop cut off after the row
// went would then never be finished as the app starts.
func TestDeleteSessionRecordsWhereItsRecordingsAreErasedFirst(t *testing.T) {
	store := newTestSQLiteStore(t)
	started := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	end := func(id, audioPath string) {
		t.Helper()
		if err := store.CreateSession(id, started); err != nil {
			t.Fatal(err)
		}
		if err := store.EndSession(id, started.Add(time.Minute), audioPath); err != nil {
			t.Fatal(err)
		}
	}
	elsewhere, audioDir := t.TempDir(), t.TempDir()
	for _, path := range []string{filepath.Join(elsewhere, "gone.mp3"), filepath.Join(audioDir, "moved.mp3")} {
		if err := os.WriteFile(path, []byte("audio"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	end("gone", filepath.Join(elsewhere, "gone.mp3")+","+filepath.Join(t.TempDir(), "missing", "moved.mp3"))
	if _, err := store.DeleteSession("gone", audioDir); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{elsewhere, audioDir} {
		if n := eraseDirectoryRows(t, store, dir); n != 1 {
			t.Errorf("%d rows of %s after the session went, want its erase directory recorded", n, dir)
		}
	}

	notADirectory := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(notADirectory, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	end("kept", filepath.Join(notADirectory, "sub", "kept.mp3"))
	if _, err := store.DeleteSession("kept", audioDir); err == nil {
		t.Error("DeleteSession deleted a session whose recording's directory it could not record")
	}
	if _, err := store.GetSession("kept"); err != nil {
		t.Errorf("the session is gone though its erase directory was not recorded: %v", err)
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

// TestEraseDirectoriesAreRecordedAsAbsolutePaths fails if a directory named
// relative to the working directory (a relative audio_dir) is recorded as
// named: its row then means another directory from another working
// directory, and it is not the row an absolute spelling of the same
// directory forgets.
func TestEraseDirectoriesAreRecordedAsAbsolutePaths(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	store := newTestSQLiteStore(t)
	if err := os.Mkdir("audio", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordEraseDirectory("audio"); err != nil {
		t.Fatal(err)
	}
	absolute := filepath.Join(root, "audio")
	if n := eraseDirectoryRows(t, store, absolute); n != 1 {
		t.Errorf("%d rows of %s after recording \"audio\", want 1 under its absolute path", n, absolute)
	}
	if err := store.ForgetEraseDirectories([]string{"audio"}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := store.DB().QueryRow(`SELECT count(*) FROM erase_directories`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d erase directories left after forgetting \"audio\", want none", n)
	}
}

// TestAnEarlierBuildsRelativeEraseDirectoryIsMadeAbsolute fails if an erase
// directory an earlier build recorded relative to the working directory (the
// session delete named a relative audio_dir so) keeps its relative name after
// the upgrade: the cleanup, which forgets absolute paths, never forgets it.
func TestAnEarlierBuildsRelativeEraseDirectoryIsMadeAbsolute(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	if err := os.Mkdir("audio", 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "erase.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`DROP TABLE erase_directories`,
		`CREATE TABLE erase_directories (path TEXT PRIMARY KEY, device INTEGER NOT NULL)`,
		`INSERT INTO erase_directories(path, device) VALUES('audio', 1)`,
	} {
		if _, err := store.DB().Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if n := eraseDirectoryRows(t, store, filepath.Join(root, "audio")); n != 1 {
		t.Errorf("%d rows of the absolute directory after the upgrade, want the relative row made absolute", n)
	}
}
