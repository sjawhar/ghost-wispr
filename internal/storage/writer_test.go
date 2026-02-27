package storage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

func TestWriterAppendsToDaily(t *testing.T) {
	dir := t.TempDir()
	w := NewWriter(dir)

	seg := transcribe.Segment{
		Speaker:   0,
		Text:      "Hello world.",
		StartTime: 0.0,
		EndTime:   1.0,
		Timestamp: time.Date(2026, 2, 26, 10, 30, 0, 0, time.Local),
	}

	if err := w.Append(seg); err != nil {
		t.Fatalf("Append failed: %v", err)
	}

	path := filepath.Join(dir, "2026-02-26.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, "Speaker 0") {
		t.Errorf("expected Speaker 0 in content, got: %s", content)
	}
	if !strings.Contains(content, "Hello world.") {
		t.Errorf("expected 'Hello world.' in content, got: %s", content)
	}
}

func TestWriterMultipleAppends(t *testing.T) {
	dir := t.TempDir()
	w := NewWriter(dir)
	ts := time.Date(2026, 2, 26, 10, 30, 0, 0, time.Local)

	_ = w.Append(transcribe.Segment{Speaker: 0, Text: "First.", Timestamp: ts})
	_ = w.Append(transcribe.Segment{Speaker: 1, Text: "Second.", Timestamp: ts})

	path := filepath.Join(dir, "2026-02-26.md")
	data, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")

	if len(lines) < 2 {
		t.Fatalf("expected at least 2 lines, got %d", len(lines))
	}
}
