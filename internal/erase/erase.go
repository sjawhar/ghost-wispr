// Package erase removes and replaces files that hold recorded audio or
// transcripts so that their bytes are overwritten with zeros, and flushed,
// before the file system frees their blocks. os.Remove and os.Rename only
// drop a name: the blocks go back to the file system's free space with the
// audio or words still in them, where a later look at the device can read
// them.
//
// Which removals zero follows Sami's answer of 2026-11-12
// (dispatch://INFRA-2961/ask/172349fa-0019-488c-9d3b-9d6b7af26a81), option
// "Zero every audio removal", with: "This is really not very important. Do
// NOT zero entire files if people just want to delete certain segments." So the
// removal of a whole audio file erases it: GC's age and disk-pressure
// removal and its raw copies, a saved session's raw recording and a partial
// encoder output use Remove, and DELETE /api/sessions/{id} uses its two
// halves, Begin before it answers and Finish after. A segment deletion zeroes
// no audio file: its splice and the copies it makes are renamed and removed
// plainly, so audio a segment deletion cuts stays recoverable at the block
// level, which that answer accepts. The database's pre-migration backup,
// which holds transcript text, is replaced and removed through Replace and
// Remove.
//
// A file being erased is first given a hidden name beside it (.erasing-*),
// and the directory is flushed, so one cut off by the app stopping, or by a
// power loss, is found and finished by Leftovers. The
// app runs Leftovers as it starts in every directory it records a recording
// or an erase in (storage.RecordingDirectories).
//
// Replace needs the file system to support hard links: it keeps the file it
// replaces under a second name until the rename is done. On one without them
// (vfat, for example) Replace fails and changes nothing: the backup is then
// not taken again, and an owed erase of it removes it instead.
//
// Limits: the zeros go to the blocks the file system has for the file, which
// a copy-on-write file system (btrfs, ZFS) would not overwrite in place, and
// below the block device a flash card's wear levelling may keep an old copy
// of a block that no file system call reaches.
package erase

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// hiddenPrefix starts the name a file has while it is being erased.
const hiddenPrefix = ".erasing-"

// ErrLeftover is wrapped by the error Remove or Replace returns when the file
// is gone from its name (Remove), or the new file has its name (Replace), but
// erasing the old content failed. The old content is left under a hidden name
// beside it, which Leftovers erases.
var ErrLeftover = errors.New("the old content is left under a hidden name (.erasing-*) for Leftovers to erase")

// Remove erases the file at path: it moves the file to a hidden name beside
// it, so path no longer names it, overwrites its bytes with zeros, flushes
// them, and removes it. An error that does not wrap ErrLeftover means path
// still names the file, unchanged. A path that is not a regular file (a
// directory, a symbolic link) holds no bytes of its own and is removed as
// os.Remove removes it.
func Remove(path string) error {
	pending, err := Begin(path)
	if err != nil && !errors.Is(err, ErrLeftover) {
		return err
	}
	// A rename Begin could not flush is flushed with the removal.
	return pending.Finish()
}

// Pending is a file Begin moved to a hidden name, for Finish to erase.
type Pending struct {
	path, hidden string
}

// Begin is the first half of Remove: it moves the file at path to a hidden
// name beside it (.erasing-*), so path no longer names it, flushes the
// directory so the rename is on disk (a power loss then cannot bring the file
// back under its name, where nothing finishes it), and returns it for Finish,
// which a caller may run later. Until Finish is done, a stop leaves the file
// under its hidden name for Leftovers. An error that does not wrap
// ErrLeftover means path still names the file, unchanged; one that does
// means the rename is done but the flush failed: the Pending returned is
// still the file's, for Finish. A path that is not a regular file is removed
// as os.Remove removes it, and Finish then has nothing to do.
func Begin(path string) (Pending, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return Pending{}, err
	}
	if !info.Mode().IsRegular() {
		return Pending{}, os.Remove(path)
	}
	dir := filepath.Dir(path)
	hidden, err := hiddenName(dir)
	if err != nil {
		return Pending{}, err
	}
	if err := os.Rename(path, hidden); err != nil {
		return Pending{}, err
	}
	pending := Pending{path: path, hidden: hidden}
	if err := syncDirectory(dir); err != nil {
		return pending, fmt.Errorf("flush the rename of %s: %w: %w", path, ErrLeftover, err)
	}
	return pending, nil
}

// Finish is the second half of Remove: it overwrites the file Begin hid with
// zeros, flushes it, and removes it. A hidden file another erase finished
// first (Orphans, Leftovers) is done: only those remove one, after zeroing
// it. Its error wraps ErrLeftover.
func (p Pending) Finish() error {
	if p.hidden == "" {
		return nil
	}
	erased := eraseFile(p.hidden)
	if errors.Is(erased, os.ErrNotExist) {
		erased = nil
	}
	if err := errors.Join(syncDirectory(filepath.Dir(p.hidden)), erased); err != nil {
		return fmt.Errorf("erase %s: %w: %w", p.path, ErrLeftover, err)
	}
	return nil
}

// Replace renames from to to, as os.Rename does, and erases the file to named
// before. That file is kept under a hidden second name until the rename is
// done, so the app stopping at any point leaves the recording whole under to
// (old or new) and the old one, once replaced, under the hidden name for
// Leftovers. An error that does not wrap ErrLeftover means nothing changed.
func Replace(from, to string) error {
	dir := filepath.Dir(to)
	old := ""
	if info, err := os.Lstat(to); err == nil && info.Mode().IsRegular() {
		if old, err = hiddenName(dir); err != nil {
			return err
		}
		if err := os.Link(to, old); err != nil {
			return fmt.Errorf("keep %s to erase it: %w", to, err)
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(from, to); err != nil {
		if old != "" {
			// to still names the old file: this name is only a second one.
			_ = os.Remove(old)
		}
		return err
	}
	if old == "" {
		return nil
	}
	if err := errors.Join(syncDirectory(dir), eraseFile(old)); err != nil {
		return fmt.Errorf("erase the file %s replaced: %w: %w", to, ErrLeftover, err)
	}
	return nil
}

// Leftovers finishes erasing the files Remove and Replace left in dir when the
// app stopped part way (.erasing-*): it overwrites with zeros each one no
// other name links and removes them all. One that another name still links is
// a second name Replace made for a file it had not replaced yet: it is only
// unlinked, and that file stays whole. Run it only while nothing else erases
// in dir (as the app starts): an erase under way keeps such a second name
// until its rename.
func Leftovers(dir string) error {
	return finish(dir, true)
}

// Orphans finishes, while the app runs, the erase of every file in dir that
// Remove or Replace left gone from its name but not overwritten
// (ErrLeftover): each .erasing-* file no other name links. A second name
// another name still links is left alone, since a Replace under way may still
// need it.
func Orphans(dir string) error {
	return finish(dir, false)
}

// finish erases the .erasing-* regular files in dir, and unlinks those another
// name still links when unlinkSecondNames is set. Anything else with that
// prefix (a symbolic link, a directory) is not one Remove or Replace makes and
// is left alone.
func finish(dir string, unlinkSecondNames bool) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", dir, err)
	}
	var errs error
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !strings.HasPrefix(entry.Name(), hiddenPrefix) {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if !unlinkSecondNames {
			info, err := os.Lstat(path)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				errs = errors.Join(errs, err)
				continue
			}
			if links(info) > 1 {
				continue
			}
		}
		// Another erase may finish the same file at once; either one's zeros
		// do.
		if err := eraseFile(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = errors.Join(errs, err)
		}
	}
	return errs
}

// eraseFile overwrites the file at path with zeros and flushes it, unless
// another name links it (its content is then still in use), and removes path.
// It does not follow a symbolic link at path.
func eraseFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR|noFollow, 0)
	if err != nil {
		return fmt.Errorf("open %s to erase it: %w", path, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if links(info) <= 1 {
		if err := zero(f, info.Size()); err != nil {
			_ = f.Close()
			return fmt.Errorf("overwrite %s: %w", path, err)
		}
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}

// zero overwrites the first size bytes of f with zeros and flushes them.
func zero(f *os.File, size int64) error {
	buf := make([]byte, 1<<20)
	for off := int64(0); off < size; {
		n := int64(len(buf))
		if size-off < n {
			n = size - off
		}
		written, err := f.WriteAt(buf[:n], off)
		if err != nil {
			return err
		}
		off += int64(written)
	}
	return f.Sync()
}

// hiddenName returns a name in dir that no file has, starting hiddenPrefix.
func hiddenName(dir string) (string, error) {
	random := make([]byte, 12)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("name a file to erase: %w", err)
	}
	return filepath.Join(dir, hiddenPrefix+hex.EncodeToString(random)), nil
}

// syncDirectory flushes a directory, so a rename or removal in it is on disk.
// Tests replace it to see when it runs.
var syncDirectory = syncDir

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}
