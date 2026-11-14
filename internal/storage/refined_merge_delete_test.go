package storage

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

func TestDeleteRefinedSegmentScrubsMergedSourcesByRecording(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "merged.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	recordingA, recordingB := "a.mp3", "b.mp3"
	for _, session := range []struct {
		id, recording string
		at            time.Time
	}{
		{"a", recordingA, started},
		{"b", recordingB, started.Add(time.Hour)},
	} {
		if err := store.CreateSession(session.id, session.at); err != nil {
			t.Fatal(err)
		}
		if err := store.EndSession(session.id, session.at.Add(time.Minute), session.recording); err != nil {
			t.Fatal(err)
		}
	}
	wordsA := []RefinedWord{{RecordingPath: recordingA, RecordingSHA256: "a", Ordinal: 0, Text: "Secret.", Start: 2, End: 3}}
	wordsB := []RefinedWord{{RecordingPath: recordingB, RecordingSHA256: "b", Ordinal: 0, Text: "Charlie stays.", Start: 0, End: 1}}
	if _, err := store.PublishRefinementAtGeneration("a", RefinedTranscript(wordsA), "nova-3", wordsA, BuildRefinedSegments(wordsA), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PublishRefinementAtGeneration("b", RefinedTranscript(wordsB), "nova-3", wordsB, BuildRefinedSegments(wordsB), 0); err != nil {
		t.Fatal(err)
	}
	if err := store.StoreEvent("live_transcript", `{"session_id":"a","type":"live_transcript","text":"Secret.","start_time":2,"end_time":3}`); err != nil {
		t.Fatal(err)
	}
	if err := store.MergeSessions("merged", []string{"a", "b"}, started, started.Add(time.Hour+time.Minute)); err != nil {
		t.Fatal(err)
	}
	words := append(append([]RefinedWord{}, wordsA...), wordsB...)
	revision, err := store.PublishRefinementAtGeneration("merged", RefinedTranscript(words), "nova-3", words, BuildRefinedSegments(words), 0)
	if err != nil {
		t.Fatal(err)
	}
	segments, err := store.GetSegments("merged")
	if err != nil {
		t.Fatal(err)
	}
	var secret int64
	for i := range segments {
		if strings.Contains(segments[i].Text, "Secret") {
			secret = segments[i].ID
		}
	}
	if err := store.DeleteRefinedSegmentText("merged", secret, revision); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{"a", "b"} {
		rows, err := store.DB().Query(`SELECT text FROM refined_words WHERE session_id = ?`, source)
		if err != nil {
			t.Fatal(err)
		}
		var text []string
		for rows.Next() {
			var word string
			if err := rows.Scan(&word); err != nil {
				t.Fatal(err)
			}
			text = append(text, word)
		}
		_ = rows.Close()
		if len(text) != 0 {
			t.Fatalf("source %s refined words = %v, want the merged session to remain authoritative", source, text)
		}
	}
	if sourceB, _ := store.GetSession("b"); !strings.Contains(sourceB.CanonicalTranscript, "Charlie") {
		t.Fatalf("source b canonical transcript = %q, want recording B text", sourceB.CanonicalTranscript)
	}
	var events int
	if err := store.DB().QueryRow(`SELECT count(*) FROM mcp_events WHERE payload LIKE '%Secret%'`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 0 {
		t.Fatalf("source live events still contain deleted text: %d", events)
	}
}

func TestRefinedDeleteKeepsAnAudiolessSourceText(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "audioless.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	startA := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	startB := startA.Add(time.Hour)
	if err := store.CreateSession("a", startA); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("a", startA.Add(time.Minute), "a.mp3"); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession("b", startB); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSegment("b", transcribe.Segment{Text: "bravo only live words", StartTime: 0, EndTime: 1, Timestamp: startB}); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("b", startB.Add(time.Minute), ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Canonicalize("b"); err != nil {
		t.Fatal(err)
	}
	if err := store.MergeSessions("merged", []string{"a", "b"}, startA, startB.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	words := []RefinedWord{
		{RecordingPath: "a.mp3", RecordingSHA256: "a", Ordinal: 0, Text: "Alpha stays.", Start: 0, End: 1},
		{RecordingPath: "a.mp3", RecordingSHA256: "a", Ordinal: 1, Text: "secret.", Start: 2, End: 3},
	}
	revision, err := store.PublishRefinementAtGeneration("merged", RefinedTranscript(words), "test", words, BuildRefinedSegments(words), 0)
	if err != nil {
		t.Fatal(err)
	}
	segments, err := store.GetSegments("merged")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteRefinedSegmentText("merged", segments[1].ID, revision); err != nil {
		t.Fatal(err)
	}
	source, err := store.GetSession("b")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(source.CanonicalTranscript, "bravo") {
		t.Errorf("audioless source text after another recording deletion = %q", source.CanonicalTranscript)
	}
}
