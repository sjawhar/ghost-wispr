package storage

import (
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

func TestR3SummaryReadyEventDropsDeletedTopic(t *testing.T) {
	store := newTestSQLiteStore(t)
	started := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	if err := store.CreateSession("s", started); err != nil {
		t.Fatal(err)
	}
	for i, text := range []string{"hello everyone", "we should talk about purple elephant seventeen today.", "ok thanks"} {
		if err := store.AppendSegment("s", transcribe.Segment{Text: text, StartTime: float64(i * 2), EndTime: float64(i*2 + 2), Timestamp: started.Add(time.Duration(i) * time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.EndSession("s", started.Add(6*time.Second), "recording.mp3"); err != nil {
		t.Fatal(err)
	}
	if err := store.StoreEvent("summary_ready", `{"type":"summary_ready","session_id":"s","title":"Purple elephant seventeen","summary":"The team discussed purple elephant seventeen."}`); err != nil {
		t.Fatal(err)
	}
	segs, _ := store.GetSegments("s")
	if err := store.DeleteSegment("s", segs[1].ID, DeletedSpan{AudioPath: "recording.mp3", Start: 2, End: 4, SHA256Before: "b", SHA256After: "a"}); err != nil {
		t.Fatal(err)
	}
	events, _ := store.GetEventsSince(0, 10)
	for _, event := range events {
		if strings.Contains(strings.ToLower(event.Payload), "purple elephant seventeen") {
			t.Fatalf("/api/events still serves the deleted topic: %s", event.Payload)
		}
	}
}

func TestR3LiveEventUsesSegmentStreamClock(t *testing.T) {
	store := newTestSQLiteStore(t)
	started := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	if err := store.CreateSession("s", started); err != nil {
		t.Fatal(err)
	}
	for _, segment := range []transcribe.Segment{{Text: "earlier person", StartTime: 2, EndTime: 4, Timestamp: started}, {Text: "private words", StartTime: 3602, EndTime: 3604, Timestamp: started.Add(time.Hour)}} {
		if err := store.AppendSegment("s", segment); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.EndSession("s", started.Add(2*time.Hour), "recording.mp3"); err != nil {
		t.Fatal(err)
	}
	_ = store.StoreEvent("live_transcript", `{"type":"live_transcript","session_id":"s","start_time":2,"end_time":4,"text":"earlier person"}`)
	_ = store.StoreEvent("live_transcript", `{"type":"live_transcript","session_id":"s","start_time":3602,"end_time":3604,"text":"private words"}`)
	segs, _ := store.GetSegments("s")
	if err := store.DeleteSegment("s", segs[1].ID, DeletedSpan{AudioPath: "recording.mp3", Start: 1.75, End: 4.25, SHA256Before: "b", SHA256After: "a"}); err != nil {
		t.Fatal(err)
	}
	events, _ := store.GetEventsSince(0, 10)
	if len(events) != 1 || !strings.Contains(events[0].Payload, "earlier person") {
		t.Fatalf("wrong live event removed; remaining %v", events)
	}
}

func TestR3BatchSummaryAtSubmitGenerationIsRefusedAfterDelete(t *testing.T) {
	store := newTestSQLiteStore(t)
	_, _ = segmentTestSession(t, store, "s")
	segs, _ := store.GetSegments("s")
	submitted, err := store.DeletionGeneration("s")
	if err != nil {
		t.Fatal(err)
	}
	_ = store.StartSummary("s")
	if err := store.DeleteSegment("s", segs[1].ID, DeletedSpan{AudioPath: "recording.mp3", Start: 2, End: 4, SHA256Before: "b", SHA256After: "a"}); err != nil {
		t.Fatal(err)
	}
	_ = store.StartSummary("s")
	err = store.UpdateSummaryAtGeneration("s", "Private deletion token", "about private deletion token", SummaryCompleted, "default", "{}", submitted)
	sess, _ := store.GetSession("s")
	if err == nil || sess.Title != "" {
		t.Fatal("stale batch summary stored")
	}
}
