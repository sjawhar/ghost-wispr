package recording

import "testing"

// TestLocalNameIsTheInverseOfFileName fails if LocalName does not return the
// file name FileName made a Drive name from, or returns one for a Drive name
// FileName cannot make: another file of the folder, an empty name, or one
// that names a path outside the directory it is joined to.
func TestLocalNameIsTheInverseOfFileName(t *testing.T) {
	for _, path := range []string{"data/audio/20261106100000.mp3", "/var/audio/20261106100000.wav", "20261106100000.mp3"} {
		name, ok := LocalName(FileName(path))
		if !ok || FileName(name) != FileName(path) || name != "20261106100000"+path[len(path)-4:] {
			t.Errorf("LocalName(FileName(%q)) = %q, %v; want the recording's file name", path, name, ok)
		}
	}
	for _, driveName := range []string{"transcript.md", "audio-", "audio-.", "audio-..", "audio-../escape.mp3", "audio-dir/x.mp3", "recording-audio-x.mp3"} {
		if name, ok := LocalName(driveName); ok {
			t.Errorf("LocalName(%q) = %q, true; want no recording", driveName, name)
		}
	}
}
