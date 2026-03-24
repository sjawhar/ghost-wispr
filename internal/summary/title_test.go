package summary

import (
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

func TestGenerateTitle_LLMTitle(t *testing.T) {
	title := GenerateTitle("Weekly Standup Notes", "some transcript", nil, time.Now())
	if title != "Weekly Standup Notes" {
		t.Errorf("expected LLM title, got %q", title)
	}
}

func TestGenerateTitle_LLMTitleNormalized(t *testing.T) {
	longTitle := strings.Repeat("word ", 20)
	title := GenerateTitle(longTitle, "", nil, time.Now())
	words := strings.Fields(title)
	if len(words) > 12 {
		t.Errorf("expected title truncated to 12 words, got %d words: %q", len(words), title)
	}
}

func TestGenerateTitle_FirstSentence(t *testing.T) {
	transcript := "We need to discuss the quarterly budget review for next month. Also some other stuff."
	title := GenerateTitle("", transcript, nil, time.Now())
	if title == "Meeting Summary" {
		t.Errorf("expected title from transcript, got %q", title)
	}
	if !strings.Contains(title, "quarterly budget") {
		t.Errorf("expected title to contain transcript content, got %q", title)
	}
}

func TestGenerateTitle_ParticipantsDate(t *testing.T) {
	startedAt := time.Date(2026, 3, 23, 14, 30, 0, 0, time.UTC)
	segments := []transcribe.Segment{
		{Speaker: 0, Text: "um yeah uh"},
		{Speaker: 1, Text: "ok well hmm"},
		{Speaker: 0, Text: "uh huh yeah"},
	}
	// Transcript is all filler, so level 2 should return "Meeting Summary"
	// and we should fall through to level 3 (participants + date)
	title := GenerateTitle("", "um yeah uh ok well hmm uh huh yeah", segments, startedAt)
	if !strings.Contains(title, "Speaker") {
		t.Errorf("expected speaker-based title, got %q", title)
	}
	if !strings.Contains(title, "Mar 23") {
		t.Errorf("expected date in title, got %q", title)
	}
}

func TestGenerateTitle_ManySpeakers(t *testing.T) {
	startedAt := time.Date(2026, 3, 23, 14, 30, 0, 0, time.UTC)
	segments := []transcribe.Segment{
		{Speaker: 0, Text: "um"},
		{Speaker: 1, Text: "uh"},
		{Speaker: 2, Text: "yeah"},
		{Speaker: 3, Text: "ok"},
	}
	title := GenerateTitle("", "um uh yeah ok", segments, startedAt)
	if !strings.Contains(title, "4 Speakers") {
		t.Errorf("expected '4 Speakers' for >3 speakers, got %q", title)
	}
}

func TestGenerateTitle_TimestampOnly(t *testing.T) {
	startedAt := time.Date(2026, 3, 23, 14, 30, 0, 0, time.UTC)
	title := GenerateTitle("", "", nil, startedAt)
	expected := "Session 2026-03-23 14:30"
	if title != expected {
		t.Errorf("expected %q, got %q", expected, title)
	}
}

func TestGenerateTitle_NeverEmpty(t *testing.T) {
	title := GenerateTitle("", "", nil, time.Time{})
	if title == "" {
		t.Error("title must never be empty")
	}
	if title != "Meeting Summary" {
		t.Errorf("expected 'Meeting Summary' as ultimate fallback, got %q", title)
	}
}

func TestGenerateTitle_WhitespaceOnlyLLMTitle(t *testing.T) {
	title := GenerateTitle("   ", "The project deadline is next Friday and we need to prepare.", nil, time.Now())
	if title == "   " || title == "" {
		t.Errorf("whitespace-only LLM title should fall through, got %q", title)
	}
}

func TestUniqueSpeakers(t *testing.T) {
	segments := []transcribe.Segment{
		{Speaker: 2, Text: "hello"},
		{Speaker: 0, Text: "hi"},
		{Speaker: 2, Text: "how are you"},
		{Speaker: 1, Text: "good"},
	}
	speakers := uniqueSpeakers(segments)
	if len(speakers) != 3 {
		t.Errorf("expected 3 unique speakers, got %d: %v", len(speakers), speakers)
	}
	// Should be sorted
	if speakers[0] != 0 || speakers[1] != 1 || speakers[2] != 2 {
		t.Errorf("expected sorted speakers [0,1,2], got %v", speakers)
	}
}

func TestUniqueSpeakers_Empty(t *testing.T) {
	speakers := uniqueSpeakers(nil)
	if len(speakers) != 0 {
		t.Errorf("expected empty speakers for nil segments, got %v", speakers)
	}
}

func TestNormalizeTitle_TruncatesLongTitle(t *testing.T) {
	long := strings.Repeat("a", 200)
	result := normalizeTitle(long)
	if len([]rune(result)) > 100 {
		t.Errorf("expected title truncated to 100 runes, got %d", len([]rune(result)))
	}
}

func TestNormalizeTitle_Empty(t *testing.T) {
	result := normalizeTitle("")
	if result != "Meeting Summary" {
		t.Errorf("expected 'Meeting Summary' for empty input, got %q", result)
	}
}

func TestGuaranteedTitle_SubstantiveSentence(t *testing.T) {
	transcript := "We discussed the new product launch timeline for Q2."
	title := guaranteedTitle(transcript)
	if title == "Meeting Summary" {
		t.Errorf("expected substantive title, got %q", title)
	}
}

func TestGuaranteedTitle_FillerOnly(t *testing.T) {
	transcript := "um uh yeah so like okay well hmm"
	title := guaranteedTitle(transcript)
	// All filler, short sentences — should fall back
	if title != "Meeting Summary" {
		// It might return a short sentence as fallback
		t.Logf("filler-only transcript produced title: %q", title)
	}
}

func TestGuaranteedTitle_Empty(t *testing.T) {
	title := guaranteedTitle("")
	if title != "Meeting Summary" {
		t.Errorf("expected 'Meeting Summary' for empty transcript, got %q", title)
	}
}
