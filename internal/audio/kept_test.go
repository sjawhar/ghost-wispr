package audio

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// saveAsCopy makes recorder save a recording by copying its raw audio to
// <session>.mp3 in dir.
func saveAsCopy(recorder *Recorder, dir string) {
	recorder.encode = func(rawPath, sessionID string) (string, error) {
		data, err := os.ReadFile(rawPath)
		if err != nil {
			return "", err
		}
		out := filepath.Join(dir, sessionID+".mp3")
		return out, os.WriteFile(out, data, 0o644)
	}
}

// KeptRawSessions lists the sessions whose raw recordings are in the audio
// directory and that the recorder is not using: not the recording under way,
// not one being saved, not one waiting for its retry. A raw recording left
// beside its saved recording is listed too.
func TestRecorderKeptRawSessions(t *testing.T) {
	dir := t.TempDir()
	recorder := NewRecorder(dir)
	for _, name := range []string{"20261106100000.pcm", "20261106120000.pcm", "20261106120000.mp3", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte{1, 2}, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "copies.pcm"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := recorder.StartSession("20261106130000"); err != nil {
		t.Fatalf("StartSession failed: %v", err)
	}
	if _, err := recorder.Writer(bytes.NewBuffer(nil)).Write([]byte{1, 2}); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	got := recorder.KeptRawSessions()
	slices.Sort(got)
	if want := []string{"20261106100000", "20261106120000"}; !slices.Equal(got, want) {
		t.Errorf("KeptRawSessions while recording = %v, want %v", got, want)
	}

	recorder.encode = func(string, string) (string, error) { return "", errors.New("no space left on device") }
	if _, err := recorder.EndSession("20261106130000"); err == nil {
		t.Fatal("EndSession saved a recording no encoder could write")
	}
	if got := recorder.KeptRawSessions(); slices.Contains(got, "20261106130000") {
		t.Errorf("KeptRawSessions = %v lists the recording waiting for its retry", got)
	}
}

// SaveKept saves a kept raw recording as a session end saves one, and keeps
// the raw file: its caller erases it once the saved recording is safe.
func TestRecorderSaveKeptKeepsTheRawRecording(t *testing.T) {
	dir := t.TempDir()
	recorder := NewRecorder(dir)
	saveAsCopy(recorder, dir)
	raw := filepath.Join(dir, "20261106100000.pcm")
	if err := os.WriteFile(raw, []byte{1, 2, 3, 4}, 0o644); err != nil {
		t.Fatal(err)
	}

	path, err := recorder.SaveKept("20261106100000")
	if err != nil {
		t.Fatalf("SaveKept failed: %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, []byte{1, 2, 3, 4}) {
		t.Errorf("saved recording = %v (%v), want the raw audio", got, err)
	}
	if got, err := os.ReadFile(raw); err != nil || !bytes.Equal(got, []byte{1, 2, 3, 4}) {
		t.Errorf("raw recording after SaveKept = %v (%v), want it kept as it was", got, err)
	}
}

// SaveKept never saves a raw recording the recorder is using, and saves
// nothing for a session without one.
func TestRecorderSaveKeptRefusesARecordingInUse(t *testing.T) {
	dir := t.TempDir()
	recorder := NewRecorder(dir)
	encodes := 0
	recorder.encode = func(string, string) (string, error) {
		encodes++
		return "", errors.New("no space left on device")
	}
	if err := recorder.StartSession("20261106130000"); err != nil {
		t.Fatalf("StartSession failed: %v", err)
	}
	if _, err := recorder.SaveKept("20261106130000"); err == nil {
		t.Error("SaveKept saved the recording under way")
	}
	if _, err := recorder.EndSession("20261106130000"); err == nil {
		t.Fatal("EndSession saved a recording no encoder could write")
	}
	encodes = 0
	if _, err := recorder.SaveKept("20261106130000"); err == nil || encodes != 0 {
		t.Errorf("SaveKept of the recording waiting for its retry = %v after %d encodes, want a refusal and none", err, encodes)
	}
	if _, err := recorder.SaveKept("20261106140000"); !errors.Is(err, os.ErrNotExist) || encodes != 0 {
		t.Errorf("SaveKept with no raw recording = %v after %d encodes, want os.ErrNotExist and none", err, encodes)
	}
}

// StartSession never reuses the id of a recording on disk: a raw recording,
// or one saved as MP3 or WAV, is left as it is, and no session starts.
func TestRecorderStartSessionRefusesARecordingOnDisk(t *testing.T) {
	for _, ext := range []string{".pcm", ".mp3", ".wav"} {
		t.Run(ext, func(t *testing.T) {
			dir := t.TempDir()
			recorder := NewRecorder(dir)
			path := filepath.Join(dir, "20261106140930"+ext)
			if err := os.WriteFile(path, []byte{1, 2, 3, 4}, 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := recorder.Writer(bytes.NewBuffer(nil)).Write([]byte{9, 9}); err != nil {
				t.Fatal(err)
			}
			if err := recorder.StartSession("20261106140930"); !errors.Is(err, os.ErrExist) {
				t.Errorf("StartSession with %s on disk = %v, want os.ErrExist", filepath.Base(path), err)
			}
			if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, []byte{1, 2, 3, 4}) {
				t.Errorf("%s after StartSession = %v (%v), want it as it was", filepath.Base(path), got, err)
			}
			recorder.mu.Lock()
			sessionID := recorder.sessionID
			recorder.mu.Unlock()
			if sessionID != "" {
				t.Errorf("StartSession refused but left session %q started", sessionID)
			}
		})
	}
}
