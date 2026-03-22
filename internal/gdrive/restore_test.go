package gdrive

import (
	"testing"
	"time"
)

func TestParseTranscriptMarkdown(t *testing.T) {
	input := `---
schema_version: 1
id: "20260322140426"
started_at: "2026-03-22T14:04:26Z"
ended_at: "2026-03-22T14:08:25Z"
speakers:
  - "Speaker 1"
---


# Transcript


**[14:04:26] Speaker 1:** Test. One, two, three.


**[14:07:21] Speaker 1:** Not assume that. We should assume everything's valuable.


**[14:07:30] Speaker 1:** On May and see what happens.
`

	fm, segments := ParseTranscriptMarkdown(input)

	if fm.ID != "20260322140426" {
		t.Fatalf("expected ID 20260322140426, got %s", fm.ID)
	}

	expectedStart, _ := time.Parse(time.RFC3339, "2026-03-22T14:04:26Z")
	if !fm.StartedAt.Equal(expectedStart) {
		t.Fatalf("expected started_at %v, got %v", expectedStart, fm.StartedAt)
	}

	if fm.EndedAt == nil {
		t.Fatal("expected ended_at to be set")
	}

	if len(segments) != 3 {
		t.Fatalf("expected 3 segments, got %d", len(segments))
	}

	if segments[0].Speaker != 0 {
		t.Fatalf("expected speaker 0 (0-indexed), got %d", segments[0].Speaker)
	}

	if segments[0].Text != "Test. One, two, three." {
		t.Fatalf("unexpected text: %s", segments[0].Text)
	}
}

func TestParseSummaryMarkdown(t *testing.T) {
	input := `---
schema_version: 1
id: "20260322140426"
started_at: "2026-03-22T14:04:26Z"
ended_at: "2026-03-22T14:08:25Z"
summary_preset: "default"
---


# 20260322140426


# Office Conversation Summary


## Key Topics
- Testing/audio check

## Action Items
- Monitor developments
`

	fm, body := ParseSummaryMarkdown(input)

	if fm.ID != "20260322140426" {
		t.Fatalf("expected ID 20260322140426, got %s", fm.ID)
	}

	if fm.SummaryPreset != "default" {
		t.Fatalf("expected preset default, got %s", fm.SummaryPreset)
	}

	// Title should be empty since it equals the session ID.
	if fm.Title != "" {
		t.Fatalf("expected empty title (ID-as-title), got %q", fm.Title)
	}

	if body == "" {
		t.Fatal("expected non-empty summary body")
	}

	if len(body) < 20 {
		t.Fatalf("summary body too short: %q", body)
	}
}

func TestParseSummaryMarkdownWithTitle(t *testing.T) {
	input := `---
schema_version: 1
id: "20260322140426"
started_at: "2026-03-22T14:04:26Z"
summary_preset: "standup"
---

# Sprint Planning Meeting

## Summary
Some planning notes here.
`

	fm, body := ParseSummaryMarkdown(input)

	if fm.Title != "Sprint Planning Meeting" {
		t.Fatalf("expected title 'Sprint Planning Meeting', got %q", fm.Title)
	}

	if fm.SummaryPreset != "standup" {
		t.Fatalf("expected preset standup, got %s", fm.SummaryPreset)
	}

	if body == "" {
		t.Fatal("expected non-empty body")
	}
}
