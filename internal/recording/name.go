package recording

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// driveNamePrefix starts the Drive name of every saved recording.
const driveNamePrefix = "audio-"

// FileName is the stable Drive name for one saved recording.
func FileName(path string) string {
	return driveNamePrefix + filepath.Base(path)
}

// LocalName returns the file name of the saved recording that the Drive file
// name names, the inverse of FileName, and false for a name FileName does not
// make: another file of the folder, or one whose rest is not a plain file
// name (empty, "." or "..", or holding a separator), which joined to a
// directory would name a path outside it.
func LocalName(name string) (string, bool) {
	base, ok := strings.CutPrefix(name, driveNamePrefix)
	if !ok || base == "" || base == "." || base == ".." || strings.ContainsRune(base, '/') || filepath.Base(base) != base {
		return "", false
	}
	return base, true
}

// Key identifies the recording a path names whether it is saved as MP3 or as
// WAV (the recorder's fallback when no MP3 encoder works): the file name
// without a .mp3 or .wav extension. Any other file name is its own key.
func Key(path string) string {
	name := filepath.Base(strings.TrimSpace(path))
	switch strings.ToLower(filepath.Ext(name)) {
	case ".mp3", ".wav":
		return strings.TrimSuffix(name, filepath.Ext(name))
	default:
		return name
	}
}

// Entries returns the recordings a session's audio path names, in order: a
// merged session's audio path names several, separated by commas. Each entry
// is trimmed, and empty ones are dropped.
func Entries(audioPath string) []string {
	var entries []string
	for entry := range strings.SplitSeq(audioPath, ",") {
		if entry = strings.TrimSpace(entry); entry != "" {
			entries = append(entries, entry)
		}
	}
	return entries
}

// RawPath is the raw capture the recorder writes for a session while it
// records, and keeps when the recording cannot be saved: <audioDir>/<id>.pcm.
func RawPath(audioDir, sessionID string) string {
	return filepath.Join(audioDir, sessionID+".pcm")
}

// Resolve returns the file one entry of a session's audio path names: the
// entry as written, when a file is there, else the file of the entry's name
// in audioDir, the audio directory now. The recorder writes
// filepath.Join(audio_dir, name), relative to the working directory when
// audio_dir is, so after audio_dir changes a recording left in place is where
// its entry points, and one moved with the setting is in the new directory.
// Every reader of a recording uses this one rule (GC, the session delete, the
// audio route, the Drive upload and the segment deletion), so all of them
// find the same file. An entry that cannot be read for another reason than
// being missing is returned as written, so its reader reports why; with no
// audio directory, so is every entry.
func Resolve(entry, audioDir string) string {
	info, err := os.Stat(entry)
	switch {
	case err == nil && !info.IsDir():
		return entry
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return entry
	case audioDir == "":
		return entry
	}
	return filepath.Join(audioDir, filepath.Base(entry))
}
