package recording

import "path/filepath"

// FileName is the stable Drive name for one saved recording.
func FileName(path string) string {
	return "audio-" + filepath.Base(path)
}
