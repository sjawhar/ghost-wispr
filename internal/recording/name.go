package recording

import (
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
	if !ok || base == "" || base == "." || base == ".." || filepath.Base(base) != base {
		return "", false
	}
	return base, true
}
