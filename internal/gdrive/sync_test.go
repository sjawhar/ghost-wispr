package gdrive

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

func TestBuildSyncFiles(t *testing.T) {
	started := time.Date(2026, 3, 21, 14, 30, 22, 0, time.UTC)
	ended := started.Add(32 * time.Minute)

	sess := SyncSession{
		ID:            "20260321-143022",
		Title:         "Weekly Standup",
		StartedAt:     started,
		EndedAt:       &ended,
		Summary:       "Team discussed sprint progress.",
		SummaryPreset: "default",
	}
	segments := []transcribe.Segment{
		{Speaker: 0, Text: "Hello", StartTime: 0.0, EndTime: 1.0, Timestamp: started},
	}

	dir := t.TempDir()
	audioPath := filepath.Join(dir, "test.mp3")
	if err := os.WriteFile(audioPath, []byte("fake-mp3"), 0o644); err != nil {
		t.Fatal(err)
	}

	files, folderName, err := BuildSyncFiles(&sess, segments, audioPath)
	if err != nil {
		t.Fatalf("build sync files: %v", err)
	}

	if folderName != "2026-03-21-weekly-standup" {
		t.Errorf("expected folder name %q, got %q", "2026-03-21-weekly-standup", folderName)
	}
	if len(files) != 3 {
		t.Fatalf("expected 3 files, got %d", len(files))
	}

	names := map[string]string{}
	for _, f := range files {
		names[f.Name] = f.MimeType
	}
	if names["summary.md"] != "application/vnd.google-apps.document" {
		t.Error("summary.md should convert to Google Doc")
	}
	if names["transcript.md"] != "application/vnd.google-apps.document" {
		t.Error("transcript.md should convert to Google Doc")
	}
	if _, ok := names["audio.mp3"]; !ok {
		t.Error("missing audio.mp3")
	}
}

func TestBuildSyncFilesNoAudio(t *testing.T) {
	started := time.Date(2026, 3, 21, 14, 30, 22, 0, time.UTC)
	ended := started.Add(32 * time.Minute)

	sess := SyncSession{
		ID:            "20260321-143022",
		Title:         "Quick Chat",
		StartedAt:     started,
		EndedAt:       &ended,
		Summary:       "Brief discussion.",
		SummaryPreset: "default",
	}
	segments := []transcribe.Segment{
		{Speaker: 0, Text: "Hello", StartTime: 0.0, EndTime: 1.0, Timestamp: started},
	}

	files, _, err := BuildSyncFiles(&sess, segments, "")
	if err != nil {
		t.Fatalf("build sync files: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 files (no audio), got %d", len(files))
	}
}

func TestBuildSyncFilesNoSummary(t *testing.T) {
	started := time.Date(2026, 3, 21, 14, 30, 22, 0, time.UTC)
	ended := started.Add(32 * time.Minute)

	sess := SyncSession{
		ID:        "20260321-143022",
		Title:     "No Summary Session",
		StartedAt: started,
		EndedAt:   &ended,
		Summary:   "",
	}
	segments := []transcribe.Segment{
		{Speaker: 0, Text: "Hello", StartTime: 0.0, EndTime: 1.0, Timestamp: started},
	}

	dir := t.TempDir()
	audioPath := filepath.Join(dir, "test.mp3")
	if err := os.WriteFile(audioPath, []byte("fake-mp3"), 0o644); err != nil {
		t.Fatal(err)
	}

	files, _, err := BuildSyncFiles(&sess, segments, audioPath)
	if err != nil {
		t.Fatalf("build sync files: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 files (no summary), got %d", len(files))
	}
	for _, f := range files {
		if f.Name == "summary.md" {
			t.Error("should not include summary.md when summary is empty")
		}
	}
}

func TestSlugify(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"Weekly Standup", "weekly-standup"},
		{"Design Review: Q1 Planning", "design-review-q1-planning"},
		{"  Spaces  Everywhere  ", "spaces-everywhere"},
		{"ALLCAPS", "allcaps"},
		{"special!@#chars$%^", "specialchars"},
		{"", "untitled"},
	}
	for _, tc := range tests {
		got := slugify(tc.input)
		if got != tc.expected {
			t.Errorf("slugify(%q) = %q, want %q", tc.input, got, tc.expected)
		}
	}
}
