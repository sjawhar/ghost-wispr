package server

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

func mergeRouteStore(t *testing.T, started time.Time, ended ...string) *storage.SQLiteStore {
	t.Helper()
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "merge-route.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for i, id := range ended {
		at := started.Add(time.Duration(i) * time.Hour)
		if err := store.CreateSession(id, at); err != nil {
			t.Fatal(err)
		}
		if err := store.EndSession(id, at.Add(time.Minute), "/audio/"+id+".mp3"); err != nil {
			t.Fatal(err)
		}
	}
	return store
}

func postMerge(t *testing.T, store *storage.SQLiteStore, ids ...string) *httptest.ResponseRecorder {
	t.Helper()
	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "", nil)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}
	body, err := json.Marshal(map[string][]string{"session_ids": ids})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/merge", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func assertMergeConflict(t *testing.T, rr *httptest.ResponseRecorder, words ...string) {
	t.Helper()
	if rr.Code != http.StatusConflict {
		t.Fatalf("merge = %d %s, want 409", rr.Code, rr.Body.String())
	}
	for _, word := range words {
		if !strings.Contains(rr.Body.String(), word) {
			t.Errorf("merge refusal %s does not name %q", rr.Body.String(), word)
		}
	}
}

func TestMergeRouteRefusesAnAlreadyMergedSessionWith409(t *testing.T) {
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	store := mergeRouteStore(t, started, "20261113090000", "20261113100000", "20261113110000")
	if err := store.MergeSessions("earlier-merge", []string{"20261113090000", "20261113100000"}, started, started.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	assertMergeConflict(t, postMerge(t, store, "20261113090000", "20261113110000"), "20261113090000", "merged into earlier-merge")
}

func TestMergeRouteRefusesARecordingSessionWith409(t *testing.T) {
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	store := mergeRouteStore(t, started, "20261113090000")
	if err := store.CreateSession("20261113100000", started.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	assertMergeConflict(t, postMerge(t, store, "20261113090000", "20261113100000"), "20261113100000", "still recording")
}

// A merge takes its id from its earliest start, so merging a merge again
// with a later session asks for the merge's own id.
func TestMergeRouteRefusesAMergeAgainUnderItsOwnIDWith409(t *testing.T) {
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	store := mergeRouteStore(t, started, "20261113090000", "20261113100000", "20261113110000")
	if rr := postMerge(t, store, "20261113090000", "20261113100000"); rr.Code != http.StatusOK {
		t.Fatalf("first merge = %d %s, want 200", rr.Code, rr.Body.String())
	}
	assertMergeConflict(t, postMerge(t, store, "20261113090000-merged", "20261113110000"), "20261113090000-merged", "already exists")
}

// Auto-merge never groups across a session that is still recording: the
// sessions before it and those after it merge as two groups, and the skip
// is logged with the session's id.
func TestAutoMergeSplitsAtARecordingSessionAndLogsTheSkip(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "auto-merge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	ids := []string{"20261113090000", "20261113090200", "20261113090400", "20261113090600", "20261113090800"}
	const recording = 2
	for i, id := range ids {
		at := started.Add(time.Duration(i) * 2 * time.Minute)
		if err := store.CreateSession(id, at); err != nil {
			t.Fatal(err)
		}
		if i == recording {
			continue
		}
		if err := store.EndSession(id, at.Add(time.Minute), "/audio/"+id+".mp3"); err != nil {
			t.Fatal(err)
		}
	}
	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "", nil)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/sessions/auto-merge?gap=300&date=2026-11-13", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("auto-merge = %d %s, want 200", rr.Code, rr.Body.String())
	}
	var result struct {
		MergedSessions []string `json:"merged_sessions"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	want := []string{ids[0] + "-merged", ids[3] + "-merged"}
	if !slices.Equal(result.MergedSessions, want) {
		t.Fatalf("auto-merge merged %q, want %q (one group on each side of the recording session)", result.MergedSessions, want)
	}
	live, err := store.GetSession(ids[recording])
	if err != nil {
		t.Fatal(err)
	}
	if live.Status != storage.SessionActive {
		t.Errorf("the recording session is %q after auto-merge, want active", live.Status)
	}
	if !strings.Contains(logs.String(), "auto-merge skipped recording session") || !strings.Contains(logs.String(), ids[recording]) {
		t.Errorf("auto-merge log = %q, want the skipped recording session named", logs.String())
	}
}
