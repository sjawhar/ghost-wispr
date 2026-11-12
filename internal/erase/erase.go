// Package erase removes and replaces files that hold recorded audio or
// transcripts so that their bytes are overwritten with zeros, and flushed,
// before the file system frees their blocks. os.Remove and os.Rename only
// drop a name: the blocks go back to the file system's free space with the
// audio or words still in them, where a later look at the device can read
// them.
//
// Erasing costs a second write of the file's size, and the unit records to an
// SD card with finite write endurance, so it is used only where a person's
// segment deletion is the reason the bytes must go: the recording a splice
// replaces, the splice's copies, recordings downloaded for a splice, and the
// database backups a deletion refreshes. Other removals stay plain os.Remove.
//
// A file being erased is first given a hidden name beside it (.erasing-*), so
// one cut off by the app stopping is found and finished by Leftovers.
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
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return os.Remove(path)
	}
	dir := filepath.Dir(path)
	hidden, err := hiddenName(dir)
	if err != nil {
		return err
	}
	if err := os.Rename(path, hidden); err != nil {
		return err
	}
	if err := errors.Join(syncDir(dir), eraseFile(hidden)); err != nil {
		return fmt.Errorf("erase %s: %w: %w", path, ErrLeftover, err)
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
	if err := errors.Join(syncDir(dir), eraseFile(old)); err != nil {
		return fmt.Errorf("erase the file %s replaced: %w: %w", to, ErrLeftover, err)
	}
	return nil
}

// Leftovers finishes erasing the files Remove and Replace left in dir when the
// app stopped part way (.erasing-*): it overwrites with zeros each one no
// other name links and removes them all. One that another name still links is
// a second name Replace made for a file it had not replaced yet: it is only
// unlinked, and that file stays whole.
func Leftovers(dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", dir, err)
	}
	var errs error
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), hiddenPrefix) {
			continue
		}
		if err := eraseFile(filepath.Join(dir, entry.Name())); err != nil {
			errs = errors.Join(errs, err)
		}
	}
	return errs
}

// eraseFile overwrites the file at path with zeros and flushes it, unless
// another name links it (its content is then still in use), and removes path.
func eraseFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
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

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}
