package audio

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// An encoder that never finishes (a wedged ffmpeg, then lame) must not hold a
// session end open: past its time limit the recording is kept as WAV. A
// capture pause waits for that end before the microphone comes back.
func TestRecorderEndSessionBoundsAWedgedEncoder(t *testing.T) {
	bin := t.TempDir()
	for _, name := range []string{"ffmpeg", "lame"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nexec /bin/sleep 3600\n"), 0o755); err != nil {
			t.Fatalf("write fake %s: %v", name, err)
		}
	}
	t.Setenv("PATH", bin)
	original := encodeTimeout
	t.Cleanup(func() { encodeTimeout = original })
	encodeTimeout = func(int64, int) time.Duration { return 100 * time.Millisecond }

	dir := t.TempDir()
	recorder := NewRecorder(dir)
	if err := recorder.StartSession("wedged"); err != nil {
		t.Fatalf("StartSession failed: %v", err)
	}
	if _, err := recorder.Writer(bytes.NewBuffer(nil)).Write(make([]byte, 3200)); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	type result struct {
		path string
		err  error
	}
	ended := make(chan result, 1)
	go func() {
		path, err := recorder.EndSession("wedged")
		ended <- result{path, err}
	}()
	var got result
	select {
	case got = <-ended:
	case <-time.After(10 * time.Second):
		t.Fatal("EndSession still waiting on a wedged encoder")
	}
	if got.err != nil {
		t.Fatalf("EndSession failed: %v", got.err)
	}
	if !strings.HasSuffix(got.path, ".wav") {
		t.Fatalf("audio path = %q, want the WAV fallback", got.path)
	}
	info, err := os.Stat(got.path)
	if err != nil || info.Size() < 3200 {
		t.Fatalf("WAV fallback missing or short: %v, %v", info, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "wedged.mp3")); !os.IsNotExist(err) {
		t.Fatalf("partial MP3 left behind: %v", err)
	}
}

func TestRecorderProducesOutputFile(t *testing.T) {
	dir := t.TempDir()
	recorder := NewRecorder(dir)

	recorder.encode = func(rawPath, sessionID string) (string, error) {
		data, err := os.ReadFile(rawPath)
		if err != nil {
			return "", err
		}
		out := filepath.Join(dir, sessionID+".mp3")
		if err := os.WriteFile(out, data, 0o644); err != nil {
			return "", err
		}
		return out, nil
	}

	if err := recorder.StartSession("abc123"); err != nil {
		t.Fatalf("StartSession failed: %v", err)
	}

	writer := recorder.Writer(bytes.NewBuffer(nil))
	if _, err := writer.Write([]byte{1, 2, 3, 4, 5, 6}); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	path, err := recorder.EndSession("abc123")
	if err != nil {
		t.Fatalf("EndSession failed: %v", err)
	}
	if path == "" {
		t.Fatal("expected output path")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat output file failed: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("expected non-empty output file")
	}
}

func TestTeeWriterWritesToBothDestinations(t *testing.T) {
	dir := t.TempDir()
	recorder := NewRecorder(dir)
	recorder.encode = func(rawPath, sessionID string) (string, error) {
		return filepath.Join(dir, sessionID+".wav"), os.WriteFile(filepath.Join(dir, sessionID+".wav"), []byte("ok"), 0o644)
	}

	if err := recorder.StartSession("tee"); err != nil {
		t.Fatalf("StartSession failed: %v", err)
	}

	var downstream bytes.Buffer
	writer := recorder.Writer(&downstream)
	payload := []byte("hello-world")
	if _, err := writer.Write(payload); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	if got := downstream.Bytes(); !bytes.Equal(got, payload) {
		t.Fatalf("downstream payload mismatch, got %q", string(got))
	}

	_, err := recorder.EndSession("tee")
	if err != nil {
		t.Fatalf("EndSession failed: %v", err)
	}

	rawBytes, err := os.ReadFile(filepath.Join(dir, "tee.pcm"))
	if err == nil && len(rawBytes) > 0 {
		t.Fatalf("expected raw pcm temp file cleanup, file still exists with %d bytes", len(rawBytes))
	}
}

func TestRecorderPrerollCapturesBeforeSessionStart(t *testing.T) {
	dir := t.TempDir()
	recorder := NewRecorder(dir)
	recorder.encode = func(rawPath, sessionID string) (string, error) {
		data, err := os.ReadFile(rawPath)
		if err != nil {
			return "", err
		}
		out := filepath.Join(dir, sessionID+".raw")
		return out, os.WriteFile(out, data, 0o644)
	}

	// Write audio data with no active session (should go to preroll buffer)
	writer := recorder.Writer(bytes.NewBuffer(nil))
	prerollData := []byte{10, 20, 30, 40, 50}
	if _, err := writer.Write(prerollData); err != nil {
		t.Fatalf("Write during no session failed: %v", err)
	}

	// Start a session — preroll should be flushed to the file
	if err := recorder.StartSession("preroll-test"); err != nil {
		t.Fatalf("StartSession failed: %v", err)
	}

	// Write more data during session
	sessionData := []byte{60, 70, 80}
	if _, err := writer.Write(sessionData); err != nil {
		t.Fatalf("Write during session failed: %v", err)
	}

	path, err := recorder.EndSession("preroll-test")
	if err != nil {
		t.Fatalf("EndSession failed: %v", err)
	}

	// Verify the output contains preroll + session data
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}

	expected := make([]byte, 0, len(prerollData)+len(sessionData))
	expected = append(expected, prerollData...)
	expected = append(expected, sessionData...)
	if !bytes.Equal(got, expected) {
		t.Errorf("got %v, expected %v", got, expected)
	}
}

// Audio held in the preroll buffer before a capture pause must not open the
// first session recorded after resume.
func TestRecorderDiscardPrerollDropsBufferedAudio(t *testing.T) {
	dir := t.TempDir()
	recorder := NewRecorder(dir)
	recorder.encode = func(rawPath, sessionID string) (string, error) {
		data, err := os.ReadFile(rawPath)
		if err != nil {
			return "", err
		}
		out := filepath.Join(dir, sessionID+".raw")
		return out, os.WriteFile(out, data, 0o644)
	}

	writer := recorder.Writer(bytes.NewBuffer(nil))
	if _, err := writer.Write([]byte{1, 2, 3, 4}); err != nil {
		t.Fatalf("Write before discard failed: %v", err)
	}
	recorder.DiscardPreroll()
	if _, err := writer.Write([]byte{9, 8}); err != nil {
		t.Fatalf("Write after discard failed: %v", err)
	}

	if err := recorder.StartSession("after-resume"); err != nil {
		t.Fatalf("StartSession failed: %v", err)
	}
	path, err := recorder.EndSession("after-resume")
	if err != nil {
		t.Fatalf("EndSession failed: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if want := []byte{9, 8}; !bytes.Equal(got, want) {
		t.Fatalf("session audio = %v, want only the audio written after the discard %v", got, want)
	}
}

// A recording EndSession could not save (every encoder and the WAV fallback
// failed, as on a full SD card) is not dropped: the next EndSession saves it,
// so a session end that is retried keeps its recording.
func TestRecorderEndSessionRetriesARecordingItCouldNotSave(t *testing.T) {
	dir := t.TempDir()
	recorder := NewRecorder(dir)
	full := true
	recorder.encode = func(rawPath, sessionID string) (string, error) {
		if full {
			return "", errors.New("no space left on device")
		}
		data, err := os.ReadFile(rawPath)
		if err != nil {
			return "", err
		}
		out := filepath.Join(dir, sessionID+".wav")
		return out, os.WriteFile(out, data, 0o644)
	}

	if err := recorder.StartSession("meeting"); err != nil {
		t.Fatalf("StartSession failed: %v", err)
	}
	if _, err := recorder.Writer(bytes.NewBuffer(nil)).Write([]byte{1, 2, 3, 4}); err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if path, err := recorder.EndSession("meeting"); err == nil {
		t.Fatalf("EndSession = %q with no encoder able to save the recording, want an error", path)
	}

	full = false
	path, err := recorder.EndSession("meeting")
	if err != nil {
		t.Fatalf("retried EndSession failed: %v", err)
	}
	if want := filepath.Join(dir, "meeting.wav"); path != want {
		t.Fatalf("retried EndSession = %q, want the recording saved at %q", path, want)
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, []byte{1, 2, 3, 4}) {
		t.Fatalf("saved recording = %v (%v), want [1 2 3 4]", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "meeting.pcm")); !os.IsNotExist(err) {
		t.Fatalf("raw recording still on disk after it was saved (stat: %v)", err)
	}
	if path, err := recorder.EndSession("meeting"); path != "" || err != nil {
		t.Fatalf("EndSession with nothing left to save = (%q, %v), want nothing", path, err)
	}
}

// When the session end gives up on a recording it cannot save, the recorder
// stops retrying it and leaves its raw audio on disk for recovery by hand,
// reporting where; with the raw file already gone, it reports none.
func TestRecorderDiscardUnsavedKeepsTheRawAudio(t *testing.T) {
	for _, tc := range []struct {
		name       string
		rawRemoved bool
	}{
		{name: "raw audio on disk"},
		{name: "raw audio deleted", rawRemoved: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			recorder := NewRecorder(dir)
			encodes := 0
			recorder.encode = func(string, string) (string, error) {
				encodes++
				return "", errors.New("no space left on device")
			}
			if err := recorder.StartSession("meeting"); err != nil {
				t.Fatalf("StartSession failed: %v", err)
			}
			if _, err := recorder.Writer(bytes.NewBuffer(nil)).Write([]byte{1, 2, 3, 4}); err != nil {
				t.Fatalf("Write failed: %v", err)
			}
			if _, err := recorder.EndSession("meeting"); err == nil {
				t.Fatal("EndSession saved a recording no encoder could write")
			}
			rawPath := filepath.Join(dir, "meeting.pcm")
			if tc.rawRemoved {
				if err := os.Remove(rawPath); err != nil {
					t.Fatalf("remove raw audio: %v", err)
				}
			}

			want := rawPath
			if tc.rawRemoved {
				want = ""
			}
			if got := recorder.DiscardUnsaved(); got != want {
				t.Fatalf("DiscardUnsaved = %q, want %q", got, want)
			}
			if path, err := recorder.EndSession("meeting"); path != "" || err != nil || encodes != 1 {
				t.Fatalf("EndSession after giving up = (%q, %v) after %d encodes, want nothing and no retry", path, err, encodes)
			}
			if !tc.rawRemoved {
				if data, err := os.ReadFile(rawPath); err != nil || !bytes.Equal(data, []byte{1, 2, 3, 4}) {
					t.Fatalf("raw audio after giving up = %v (%v), want it kept", data, err)
				}
			}
		})
	}
}

// When the preroll cannot be written into a new session's raw file (a full
// card), StartSession fails and leaves no session open: it closes the file
// instead of appending every later buffer to a recording whose session the
// manager has already rolled back, and later audio goes to the preroll again.
func TestRecorderStartSessionLeavesNoSessionOpenWhenThePrerollCannotBeWritten(t *testing.T) {
	if _, err := os.Stat("/dev/full"); err != nil {
		t.Skip("needs /dev/full to make the preroll write fail")
	}
	dir := t.TempDir()
	if err := os.Symlink("/dev/full", filepath.Join(dir, "meeting.pcm")); err != nil {
		t.Fatal(err)
	}
	recorder := NewRecorder(dir)
	writer := recorder.Writer(bytes.NewBuffer(nil))
	if _, err := writer.Write([]byte{1, 2, 3, 4}); err != nil {
		t.Fatalf("Write before the session: %v", err)
	}

	if err := recorder.StartSession("meeting"); err == nil {
		t.Fatal("StartSession succeeded although the preroll could not be written")
	}
	recorder.mu.Lock()
	sessionID, rawFile := recorder.sessionID, recorder.rawFile
	recorder.mu.Unlock()
	if sessionID != "" || rawFile != nil {
		t.Fatalf("StartSession failed but left session %q with its file open", sessionID)
	}
	if _, err := writer.Write([]byte{5, 6}); err != nil {
		t.Fatalf("Write after the failed start went to the session's file: %v", err)
	}
}

// saveAsWAV makes recorder save a recording by copying its raw audio to
// <session>.wav in dir, failing while *full is set.
func saveAsWAV(recorder *Recorder, dir string, full *bool) {
	recorder.encode = func(rawPath, sessionID string) (string, error) {
		if *full {
			return "", errors.New("no space left on device")
		}
		data, err := os.ReadFile(rawPath)
		if err != nil {
			return "", err
		}
		out := filepath.Join(dir, sessionID+".wav")
		return out, os.WriteFile(out, data, 0o644)
	}
}

// EndSession ends only the session it names. Ending a session that failed to
// start returns nothing, never the recording of an earlier session whose save
// failed: that one stays for its own session's retry.
func TestRecorderEndSessionNeverReturnsAnotherSessionsRecording(t *testing.T) {
	if _, err := os.Stat("/dev/full"); err != nil {
		t.Skip("needs /dev/full to make the preroll write fail")
	}
	dir := t.TempDir()
	recorder := NewRecorder(dir)
	full := true
	saveAsWAV(recorder, dir, &full)
	writer := recorder.Writer(bytes.NewBuffer(nil))

	if err := recorder.StartSession("earlier"); err != nil {
		t.Fatalf("StartSession(earlier) failed: %v", err)
	}
	if _, err := writer.Write([]byte{1, 2, 3, 4}); err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if _, err := recorder.EndSession("earlier"); err == nil {
		t.Fatal("EndSession(earlier) saved a recording no encoder could write")
	}

	if err := os.Symlink("/dev/full", filepath.Join(dir, "later.pcm")); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte{5, 6}); err != nil {
		t.Fatalf("Write before the later session: %v", err)
	}
	if err := recorder.StartSession("later"); err == nil {
		t.Fatal("StartSession(later) succeeded although the preroll could not be written")
	}

	full = false
	if path, err := recorder.EndSession("later"); path != "" || err != nil {
		t.Fatalf("EndSession(later) = (%q, %v), want nothing: that session never recorded", path, err)
	}
	path, err := recorder.EndSession("earlier")
	if err != nil {
		t.Fatalf("retried EndSession(earlier) failed: %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, []byte{1, 2, 3, 4}) {
		t.Fatalf("earlier session's recording = %v (%v), want [1 2 3 4]", got, err)
	}
}

// KeptRawRecordings counts the raw recordings kept in the audio directory
// because they could not be saved, with their total size: not the recording
// under way, not one being saved or waiting for its retry, and not one left
// beside its saved recording. One given up after failed saves counts.
func TestRecorderKeptRawRecordings(t *testing.T) {
	dir := t.TempDir()
	recorder := NewRecorder(dir)
	for name, size := range map[string]int{
		"20261106100000.pcm": 1000, // kept: no saved recording
		"20261106110000.pcm": 2000, // kept
		"20261106120000.pcm": 4000, // left beside its saved recording
		"20261106120000.mp3": 10,
		"notes.txt":          50,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "copies.pcm"), 0o755); err != nil {
		t.Fatal(err)
	}
	expect := func(when string, wantCount int, wantBytes int64) {
		t.Helper()
		if count, size := recorder.KeptRawRecordings(); count != wantCount || size != wantBytes {
			t.Errorf("%s: KeptRawRecordings = (%d, %d), want (%d, %d)", when, count, size, wantCount, wantBytes)
		}
	}

	if err := recorder.StartSession("20261106130000"); err != nil {
		t.Fatalf("StartSession failed: %v", err)
	}
	if _, err := recorder.Writer(bytes.NewBuffer(nil)).Write(make([]byte, 8000)); err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	expect("while recording", 2, 3000)

	recorder.encode = func(string, string) (string, error) {
		expect("while saving", 2, 3000)
		return "", errors.New("no space left on device")
	}
	if _, err := recorder.EndSession("20261106130000"); err == nil {
		t.Fatal("EndSession saved a recording no encoder could write")
	}
	expect("waiting for its retry", 2, 3000)

	recorder.DiscardUnsaved()
	expect("given up", 3, 11000)
}
