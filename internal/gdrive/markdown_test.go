package gdrive

import (
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

func TestRenderSummaryMarkdown(t *testing.T) {
	started := time.Date(2026, 3, 21, 14, 30, 22, 0, time.UTC)
	ended := started.Add(32 * time.Minute)

	md := RenderSummaryMarkdown(SyncSession{
		ID:            "20260321-143022",
		Title:         "Weekly Standup",
		StartedAt:     started,
		EndedAt:       &ended,
		Summary:       "Team discussed sprint progress.",
		SummaryPreset: "default",
	})

	if !strings.Contains(md, "schema_version: 1") {
		t.Error("missing schema_version")
	}
	if !strings.Contains(md, `id: "20260321-143022"`) {
		t.Error("missing id in frontmatter")
	}
	if !strings.Contains(md, "# Weekly Standup") {
		t.Error("missing title heading")
	}
	if !strings.Contains(md, "Team discussed sprint progress.") {
		t.Error("missing summary body")
	}
}

func TestRenderTranscriptMarkdown(t *testing.T) {
	started := time.Date(2026, 3, 21, 14, 30, 22, 0, time.UTC)
	ended := started.Add(32 * time.Minute)

	segments := []transcribe.Segment{
		{Speaker: 0, Text: "Hello everyone", StartTime: 0.0, EndTime: 1.5, Timestamp: started},
		{Speaker: 1, Text: "Hi, let's begin", StartTime: 2.0, EndTime: 3.5, Timestamp: started.Add(2 * time.Second)},
	}

	md := RenderTranscriptMarkdown(SyncSession{
		ID:        "20260321-143022",
		Title:     "Weekly Standup",
		StartedAt: started,
		EndedAt:   &ended,
	}, segments)

	if !strings.Contains(md, "schema_version: 1") {
		t.Error("missing schema_version")
	}
	if !strings.Contains(md, "# Transcript") {
		t.Error("missing transcript heading")
	}
	if !strings.Contains(md, "Speaker 1") {
		t.Error("missing speaker label")
	}
	if !strings.Contains(md, "Hello everyone") {
		t.Error("missing segment text")
	}
	if !strings.Contains(md, "- \"Speaker 1\"") {
		t.Error("missing speaker in frontmatter")
	}
}
