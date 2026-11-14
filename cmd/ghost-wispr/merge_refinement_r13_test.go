package main

import (
	"context"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// TestR13MergeRouteRefinesBeforeDeletion fails if a normal merge route leaves
// the new conversation pending forever instead of producing deletable segments.
func TestR13MergeRouteRefinesBeforeDeletion(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "merge-route.db"))
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
		if err := store.EndSession(id, at.Add(time.Minute), filepath.Join(t.TempDir(), id+".mp3")); err != nil {
			t.Fatal(err)
		}
	}
	refined := make(chan struct{}, 1)
	h, err := server.Handler(fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("ok"), Mode: fs.ModePerm}}, server.NewHub(), store, &server.ControlHooks{
		OnSessionMerged: func(_ context.Context, sessionID string) {
			session, err := store.GetSession(sessionID)
			if err != nil {
				t.Errorf("get merged session: %v", err)
				return
			}
			paths := strings.Split(session.AudioPath, ",")
			words := make([]storage.RefinedWord, 0, len(paths))
			for i, path := range paths {
				words = append(words, storage.RefinedWord{RecordingPath: path, RecordingSHA256: path, RecordingOrdinal: i, Ordinal: 0, Text: "merged words", Start: 0, End: 1})
			}
			if _, err := store.PublishRefinementAtGeneration(sessionID, storage.RefinedTranscript(words), "test", words, storage.BuildRefinedSegments(words), 0); err != nil {
				t.Errorf("publish merged refinement: %v", err)
				return
			}
			refined <- struct{}{}
		},
		DeleteSegmentText: func(_ context.Context, sessionID string, segmentID int64, revision int) error {
			return store.DeleteRefinedSegmentText(sessionID, segmentID, revision)
		},
	}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	merge := httptest.NewRecorder()
	h.ServeHTTP(merge, httptest.NewRequest(http.MethodPost, "/api/sessions/merge", strings.NewReader(`{"session_ids":["a","b"]}`)))
	if merge.Code != http.StatusOK {
		t.Fatalf("merge = %d %s", merge.Code, merge.Body.String())
	}
	select {
	case <-refined:
	case <-time.After(time.Second):
		t.Fatal("merge route did not refine the merged session")
	}
	mergedID := started.UTC().Format("20060102150405") + "-merged"
	merged, err := store.GetSession(mergedID)
	if err != nil || merged.RefinementStatus != storage.RefinementCompleted || merged.RefinedRevision == 0 {
		t.Fatalf("merged session = %+v, %v; want completed refinement", merged, err)
	}
	segments, err := store.GetSegments(mergedID)
	if err != nil || len(segments) == 0 {
		t.Fatalf("merged segments = %v, %v", segments, err)
	}
	deleteRequest := httptest.NewRecorder()
	h.ServeHTTP(deleteRequest, httptest.NewRequest(http.MethodDelete, "/api/sessions/"+mergedID+"/segments/"+strconv.FormatInt(segments[0].ID, 10)+"?mode=text&revision="+strconv.Itoa(merged.RefinedRevision), nil))
	if deleteRequest.Code != http.StatusNoContent {
		t.Fatalf("merged segment delete = %d %s, want 204", deleteRequest.Code, deleteRequest.Body.String())
	}
}
