package transcribe

import (
	"testing"
	"time"
)

func intPtr(i int) *int { return &i }

func TestGroupWordsBySpeker(t *testing.T) {
	words := []Word{
		{Speaker: intPtr(0), PunctuatedWord: "Hello", Start: 0.0, End: 0.5},
		{Speaker: intPtr(0), PunctuatedWord: "world.", Start: 0.5, End: 1.0},
		{Speaker: intPtr(1), PunctuatedWord: "Hi", Start: 1.2, End: 1.5},
		{Speaker: intPtr(1), PunctuatedWord: "there.", Start: 1.5, End: 2.0},
		{Speaker: intPtr(0), PunctuatedWord: "How", Start: 2.2, End: 2.5},
		{Speaker: intPtr(0), PunctuatedWord: "are", Start: 2.5, End: 2.7},
		{Speaker: intPtr(0), PunctuatedWord: "you?", Start: 2.7, End: 3.0},
	}

	segments := GroupWordsBySpeaker(words)

	if len(segments) != 3 {
		t.Fatalf("expected 3 segments, got %d", len(segments))
	}
	if segments[0].Speaker != 0 || segments[0].Text != "Hello world." {
		t.Errorf("segment 0: got speaker=%d text=%q", segments[0].Speaker, segments[0].Text)
	}
	if segments[1].Speaker != 1 || segments[1].Text != "Hi there." {
		t.Errorf("segment 1: got speaker=%d text=%q", segments[1].Speaker, segments[1].Text)
	}
	if segments[2].Speaker != 0 || segments[2].Text != "How are you?" {
		t.Errorf("segment 2: got speaker=%d text=%q", segments[2].Speaker, segments[2].Text)
	}
}

func TestGroupWordsNilSpeaker(t *testing.T) {
	words := []Word{
		{Speaker: nil, PunctuatedWord: "Hello", Start: 0.0, End: 0.5},
	}
	segments := GroupWordsBySpeaker(words)
	if len(segments) != 1 {
		t.Fatalf("expected 1 segment, got %d", len(segments))
	}
	if segments[0].Speaker != -1 {
		t.Errorf("expected speaker -1 for nil, got %d", segments[0].Speaker)
	}
}

func TestFormatSegmentMarkdown(t *testing.T) {
	seg := Segment{
		Speaker:   0,
		Text:      "Hello world.",
		StartTime: 1.5,
		EndTime:   3.0,
		Timestamp: time.Date(2026, 2, 26, 10, 32, 15, 0, time.Local),
	}
	got := seg.FormatMarkdown()
	want := "**[10:32:15] Speaker 0:** Hello world."
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
