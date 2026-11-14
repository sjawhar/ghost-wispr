package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

func TestMergedSourceSegmentsRouteHidesStaleRows(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "merged-source.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	for i, id := range []string{"a", "b"} {
		at := started.Add(time.Duration(i) * time.Hour)
		if err := store.CreateSession(id, at); err != nil {
			t.Fatal(err)
		}
		if err := store.EndSession(id, at.Add(time.Minute), "/audio/"+id+".mp3"); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.MergeSessions("merged", []string{"a", "b"}, started, started.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`INSERT INTO segments(session_id, speaker, text, start_time, end_time, timestamp) VALUES('a', 0, 'stale source text', 0, 1, ?)`, started.UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	handler, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/sessions/a/segments", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("merged source segments = %d %s", response.Code, response.Body.String())
	}
	var segments []any
	if err := json.Unmarshal(response.Body.Bytes(), &segments); err != nil {
		t.Fatal(err)
	}
	if len(segments) != 0 {
		t.Fatalf("merged source segments = %#v, want none", segments)
	}
}

func TestMergedDetailSendsZeroRevisionForUntimedSegments(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "merged-untimed.json.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	if err := store.CreateSession("without-recording", started); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSegment("without-recording", transcribe.Segment{Text: "untimed source text", StartTime: 0, EndTime: 1, Timestamp: started}); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("without-recording", started.Add(time.Minute), ""); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession("with-recording", started.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("with-recording", started.Add(61*time.Minute), "/audio/with-recording.mp3"); err != nil {
		t.Fatal(err)
	}
	if err := store.MergeSessions("merged", []string{"without-recording", "with-recording"}, started, started.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	handler, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/sessions/merged", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("merged detail = %d %s", response.Code, response.Body.String())
	}
	var detail struct {
		Segments []map[string]json.RawMessage `json:"segments"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	for _, segment := range detail.Segments {
		if string(segment["text"]) == `"untimed source text"` {
			if string(segment["transcript_revision"]) != "0" {
				t.Fatalf("untimed segment revision JSON = %s, want 0", segment["transcript_revision"])
			}
			return
		}
	}
	t.Fatal("merged detail has no untimed source segment")
}
