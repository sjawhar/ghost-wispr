package audio

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

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

	path, err := recorder.EndSession()
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

	_, err := recorder.EndSession()
	if err != nil {
		t.Fatalf("EndSession failed: %v", err)
	}

	rawBytes, err := os.ReadFile(filepath.Join(dir, "tee.pcm"))
	if err == nil && len(rawBytes) > 0 {
		t.Fatalf("expected raw pcm temp file cleanup, file still exists with %d bytes", len(rawBytes))
	}
}
