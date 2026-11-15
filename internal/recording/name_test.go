package recording

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

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
	for _, driveName := range []string{"transcript.md", "audio-", "audio-.", "audio-..", "audio-/", "audio-../escape.mp3", "audio-dir/x.mp3", "recording-audio-x.mp3"} {
		if name, ok := LocalName(driveName); ok {
			t.Errorf("LocalName(%q) = %q, true; want no recording", driveName, name)
		}
	}
}

// TestKeyNamesARecordingAsMP3OrWAV fails if Key gives a recording's MP3 and
// WAV forms different keys, or drops an extension that is not one of them.
func TestKeyNamesARecordingAsMP3OrWAV(t *testing.T) {
	for _, tc := range []struct{ path, want string }{
		{"data/audio/20261106100000.mp3", "20261106100000"},
		{" /var/audio/20261106100000.WAV ", "20261106100000"},
		{"20261106100000.pcm", "20261106100000.pcm"},
		{"20261106100000", "20261106100000"},
		{"", "."},
	} {
		if got := Key(tc.path); got != tc.want {
			t.Errorf("Key(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

// TestEntriesSplitsAMergedAudioPath fails if Entries does not return each
// recording of an audio path, trimmed and in order, without empty entries.
func TestEntriesSplitsAMergedAudioPath(t *testing.T) {
	for _, tc := range []struct {
		audioPath string
		want      []string
	}{
		{"", nil},
		{" , ,", nil},
		{"a.mp3", []string{"a.mp3"}},
		{"data/a.mp3, data/b.wav,,c.mp3 ", []string{"data/a.mp3", "data/b.wav", "c.mp3"}},
	} {
		if got := Entries(tc.audioPath); !slices.Equal(got, tc.want) {
			t.Errorf("Entries(%q) = %q, want %q", tc.audioPath, got, tc.want)
		}
	}
}

func TestRawPathIsTheSessionsPCMInTheAudioDirectory(t *testing.T) {
	if got, want := RawPath("data/audio", "20261106100000"), filepath.Join("data", "audio", "20261106100000.pcm"); got != want {
		t.Fatalf("RawPath = %q, want %q", got, want)
	}
}

// TestResolveReadsTheEntryAsWrittenThenTheAudioDirectory fails if Resolve
// does not return a recording where its entry points (relative to the
// working directory, as the recorder writes it, or absolute) while a file is
// there, or does not look for the entry's name in the audio directory when
// none is: after audio_dir changes, a recording left in place and one moved
// with the setting must both be found.
func TestResolveReadsTheEntryAsWrittenThenTheAudioDirectory(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	audioDir := filepath.Join(root, "new-audio")
	for _, dir := range []string{"old-audio", audioDir, filepath.Join(root, "a-directory.mp3")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	leftInPlace := filepath.Join("old-audio", "20261106100000.mp3")
	absolute := filepath.Join(root, "old-audio", "20261106110000.mp3")
	for _, path := range []string{leftInPlace, absolute, filepath.Join(audioDir, "20261106120000.mp3")} {
		if err := os.WriteFile(path, []byte("audio"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct{ entry, audioDir, want string }{
		{leftInPlace, audioDir, leftInPlace},
		{absolute, audioDir, absolute},
		{filepath.Join("old-audio", "20261106120000.mp3"), audioDir, filepath.Join(audioDir, "20261106120000.mp3")},
		{filepath.Join(root, "gone", "20261106120000.mp3"), audioDir, filepath.Join(audioDir, "20261106120000.mp3")},
		{filepath.Join(root, "a-directory.mp3"), audioDir, filepath.Join(audioDir, "a-directory.mp3")},
		{filepath.Join("old-audio", "missing.mp3"), "", filepath.Join("old-audio", "missing.mp3")},
	} {
		if got := Resolve(tc.entry, tc.audioDir); got != tc.want {
			t.Errorf("Resolve(%q, %q) = %q, want %q", tc.entry, tc.audioDir, got, tc.want)
		}
	}
}
