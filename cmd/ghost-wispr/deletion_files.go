package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/sjawhar/ghost-wispr/internal/erase"
)

// syncRecordingDirectory flushes the directory holding recording, so the
// rename that put the spliced recording there is on disk before the splice is
// marked complete: after a power loss the mark must not stand beside the
// unspliced recording, which the Drive replacement would then upload as the
// clean copy.
var syncRecordingDirectory = func(recording string) error {
	dir := filepath.Dir(recording)
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open recording directory %s: %w", dir, err)
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("flush recording directory %s: %w", dir, err)
	}
	return nil
}

// removeFiles removes paths with a plain unlink: a segment deletion zeroes
// no whole audio file (Sami, 2026-11-12, dispatch://INFRA-2961/ask/172349fa-0019-488c-9d3b-9d6b7af26a81).
func removeFiles(paths []string) {
	for _, path := range paths {
		_ = os.Remove(path)
	}
}

// cleanupSpliceTempCopiesExcept removes, as the app starts and before any
// deletion or recovery runs, the temporary files a stopped app can leave in
// audioDir: a splice's copy (.delete-segment-*), output (.splice-*), and a
// recording downloaded from Drive for a splice (.download-*). It removes them
// with a plain unlink, since a segment deletion zeroes no whole audio file,
// and finishes overwriting any cut-off whole-file removal, except a reserved
// hidden raw capture whose session still exists and could not be restored.
func cleanupSpliceTempCopiesExcept(audioDir string, reserved map[string]bool) error {
	entries, err := os.ReadDir(audioDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read audio directory %s: %w", audioDir, err)
	}
	var cleanupErr error
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, ".delete-segment-") && !strings.HasPrefix(name, ".splice-") && !strings.HasPrefix(name, ".download-") {
			continue
		}
		if err := os.Remove(filepath.Join(audioDir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			cleanupErr = errors.Join(cleanupErr, err)
		}
	}
	return errors.Join(cleanupErr, erase.LeftoversExcept(audioDir, func(path string) bool { return reserved[path] }))
}

// recordingDirectoryLister lists the directories recordings are kept or
// erased in outside the audio directory, forgets the erase directories the
// startup cleanup has finished, and records the audio directory as one.
type recordingDirectoryLister interface {
	RecordingDirectories() ([]string, error)
	ReservedRawErasePaths() (map[string]bool, error)
	ForgetEraseDirectories(dirs []string) error
	RecordEraseDirectory(dir string) error
}

// cleanupRecordingDirectories runs cleanupSpliceTempCopiesExcept, as the app
// starts, in audioDir and in every directory a session keeps a recording in
// or a whole-file erase was started in (storage.RecordingDirectories):
// splice copies, downloads and cut-off erasures are made beside the
// recording they copy or erase. Each directory is named by its absolute path,
// as the store lists it, so a relative audio_dir is cleaned once. It then
// hands the directories it cleaned without an error to
// ForgetEraseDirectories, which forgets those that were there to clean, and
// last records audioDir as an erase directory: the recorder erases a saved
// session's raw recording there without recording where, so an erase of it a
// stop cuts off is still finished at a start with another audio_dir.
func cleanupRecordingDirectories(store recordingDirectoryLister, audioDir string) error {
	audioDir, err := filepath.Abs(audioDir)
	if err != nil {
		return fmt.Errorf("find the audio directory: %w", err)
	}
	reserved, err := store.ReservedRawErasePaths()
	if err != nil {
		return fmt.Errorf("list reserved raw erases: %w", err)
	}
	dirs := []string{audioDir}
	others, err := store.RecordingDirectories()
	if err != nil {
		return errors.Join(err, cleanupSpliceTempCopiesExcept(audioDir, reserved), store.RecordEraseDirectory(audioDir))
	}
	for _, dir := range others {
		if dir, err = filepath.Abs(dir); err != nil {
			return fmt.Errorf("find the recording directory: %w", err)
		}
		if !slices.Contains(dirs, dir) {
			dirs = append(dirs, dir)
		}
	}
	var cleanupErr error
	var cleaned []string
	for _, dir := range dirs {
		if err := cleanupSpliceTempCopiesExcept(dir, reserved); err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
			continue
		}
		cleaned = append(cleaned, dir)
	}
	return errors.Join(cleanupErr, store.ForgetEraseDirectories(cleaned), store.RecordEraseDirectory(audioDir))
}

func downloadDestination(recording string) (string, error) {
	output, err := os.CreateTemp(filepath.Dir(recording), ".download-*"+filepath.Ext(recording))
	if err != nil {
		return "", fmt.Errorf("create recovery download beside %s: %w", recording, err)
	}
	path := output.Name()
	if err := output.Close(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("close recovery download %s: %w", path, err)
	}
	return path, nil
}

func copyForSplice(source string) (string, error) {
	input, err := os.Open(source)
	if err != nil {
		return "", fmt.Errorf("open recording %s: %w", source, err)
	}
	defer func() { _ = input.Close() }()
	ext := filepath.Ext(source)
	output, err := os.CreateTemp(filepath.Dir(source), ".delete-segment-*"+ext)
	if err != nil {
		return "", fmt.Errorf("create splice copy beside %s: %w", source, err)
	}
	path := output.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(path)
		}
	}()
	if _, err = io.Copy(output, input); err != nil {
		_ = output.Close()
		return "", fmt.Errorf("copy recording %s: %w", source, err)
	}
	if err = output.Sync(); err != nil {
		_ = output.Close()
		return "", fmt.Errorf("flush splice copy %s: %w", path, err)
	}
	if err = output.Close(); err != nil {
		return "", fmt.Errorf("close splice copy %s: %w", path, err)
	}
	return path, nil
}
