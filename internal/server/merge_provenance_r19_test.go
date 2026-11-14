package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// A deletion refused because a merged session holds a row that no source can
// be credited with answers 409 with a sentence that says so: the conversation
// is refined, so "still being refined" would not be true.
func TestDeleteRouteNamesAnUntraceableMergeHistory(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "provenance.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	start := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	segment := func(text string) transcribe.Segment {
		return transcribe.Segment{Speaker: 0, Text: text, StartTime: 0, EndTime: 1, Timestamp: start}
	}
	for _, id := range []string{"n", "s"} {
		if err := store.CreateSession(id, start); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.AppendSegment("n", segment("november delete")); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("n", start.Add(time.Minute), ""); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSegment("s", segment("untraceable holder text")); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("s", start.Add(time.Minute), "/audio/s.mp3"); err != nil {
		t.Fatal(err)
	}
	if err := store.MergeSessions("m1", []string{"n", "s"}, start, start.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession("t", start.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("t", start.Add(181*time.Minute), "/audio/t.mp3"); err != nil {
		t.Fatal(err)
	}
	if err := store.MergeSessions("m2", []string{"m1", "t"}, start, start.Add(4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE segments SET source_session_id = '', recording_path = '' WHERE session_id = 'm2' AND text = 'untraceable holder text'`); err != nil {
		t.Fatal(err)
	}
	segments, err := store.GetSegments("m2")
	if err != nil {
		t.Fatal(err)
	}
	var target int64
	for i := range segments {
		if segments[i].Text == "november delete" {
			target = segments[i].ID
		}
	}
	if target == 0 {
		t.Fatal("control: the untimed row is missing from m2")
	}
	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{
		DeleteSegmentText: func(_ context.Context, sessionID string, segmentID int64, revision int) error {
			return store.DeleteRefinedSegmentText(sessionID, segmentID, revision)
		},
	}, "", nil)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, fmt.Sprintf("/api/sessions/m2/segments/%d?mode=text&revision=0", target), nil))
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "merge history can't be traced") {
		t.Fatalf("delete with an untraceable merge history = %d %s, want 409 saying the merge history can't be traced", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "still being refined") {
		t.Fatalf("delete with an untraceable merge history says the conversation is still being refined: %s", rr.Body.String())
	}
}
