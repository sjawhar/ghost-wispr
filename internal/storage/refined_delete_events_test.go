package storage

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDeleteRefinedSegmentDropsLiveTranscriptEvents(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	if err := store.CreateSession("s", started); err != nil {
		t.Fatal(err)
	}
	if err := store.StoreEvent("live_transcript", `{"type":"live_transcript","session_id":"s","text":"live secret words","start_time":2,"end_time":4}`); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("s", started.Add(time.Minute), "recording.mp3"); err != nil {
		t.Fatal(err)
	}
	words := []RefinedWord{
		{RecordingPath: "recording.mp3", RecordingSHA256: "hash", Ordinal: 0, Text: "Before.", Start: 0, End: 1},
		{RecordingPath: "recording.mp3", RecordingSHA256: "hash", Ordinal: 1, Text: "Um,", Start: 2, End: 2.2},
		{RecordingPath: "recording.mp3", RecordingSHA256: "hash", Ordinal: 2, Text: "secret", Start: 2.2, End: 3},
		{RecordingPath: "recording.mp3", RecordingSHA256: "hash", Ordinal: 3, Text: "words.", Start: 3, End: 4},
		{RecordingPath: "recording.mp3", RecordingSHA256: "hash", Ordinal: 4, Text: "After.", Start: 5, End: 6},
	}
	revision, err := store.PublishRefinementAtGeneration("s", RefinedTranscript(words), "nova-3", words, BuildRefinedSegments(words), 0)
	if err != nil {
		t.Fatal(err)
	}
	segments, err := store.GetSegments("s")
	if err != nil {
		t.Fatal(err)
	}
	var selected int64
	for i := range segments {
		if strings.Contains(segments[i].Text, "secret") {
			selected = segments[i].ID
		}
	}
	if selected == 0 {
		t.Fatal("control: no refined secret segment")
	}
	if err := store.DeleteRefinedSegmentText("s", selected, revision); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.DB().QueryRow(`SELECT count(*) FROM mcp_events WHERE payload LIKE '%secret%'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("stored live events still hold deleted words: %d", count)
	}
}
