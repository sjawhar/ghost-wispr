package main

// A refused "Delete text and audio" leaves the deleted words in the
// recording, and its /api/status warning is the only sign of that. These
// tests drive the real HTTP handler with the hooks main.go wires
// (wireSegmentDeletion), restarting the unit by reopening its database.

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// refusingRunner makes the deletion runner main.go makes, over store, for
// refusedCutFixture's recording, with a check after each cut that still hears
// the segment's "tractor" at the cut.
func refusingRunner(store *storage.SQLiteStore, recording string) func() *segmentDeleteRunner {
	return func() *segmentDeleteRunner {
		return &segmentDeleteRunner{
			store: store,
			ensureRecordings: func(context.Context, storage.Session) (deletionRecordingSet, error) {
				return deletionRecordingSet{Paths: []string{recording}}, nil
			},
			splice: func(context.Context, string, float64, float64) (string, error) { return "after", nil },
			transcribeCut: func(context.Context, string, string) ([]transcribe.TimestampedWord, error) {
				return []transcribe.TimestampedWord{{Text: "So", Start: 0.2, End: 0.5}, {Text: "tractor", Start: 62.4, End: 62.7}}, nil
			},
		}
	}
}

// unitServer is the HTTP handler a unit starting on store serves, as far as
// segment deletion and /api/status go.
func unitServer(t *testing.T, store *storage.SQLiteStore, recording string) http.Handler {
	t.Helper()
	hooks := &server.ControlHooks{}
	wireSegmentDeletion(hooks, newWarningsRegistry(), newRefusedCuts(store), refusingRunner(store, recording))
	handler, err := server.Handler(fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("ok"), Mode: fs.ModePerm}}, server.NewHub(), store, hooks, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func serve(handler http.Handler, method, target string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(method, target, nil))
	return recorder
}

func apiWarnings(t *testing.T, handler http.Handler) []string {
	t.Helper()
	recorder := serve(handler, http.MethodGet, "/api/status")
	var response struct {
		Warnings []string `json:"warnings"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("decode /api/status (%d): %v", recorder.Code, err)
	}
	return response.Warnings
}

// isRefusedCutWarning reports whether warning is refusedCutFixture's refused
// cut's: its session and time, and none of its words.
func isRefusedCutWarning(warning string) bool {
	lower := strings.ToLower(warning)
	return strings.Contains(warning, `"Delete text and audio" was refused`) && strings.Contains(warning, "session s") &&
		strings.Contains(warning, "1:03–1:04") && !strings.Contains(lower, "tractor") && !strings.Contains(lower, "orange")
}

// TestRefusedCutWarningSurvivesARestart fails if the status warning of a
// refused "Delete text and audio", made through the server main.go wires, is
// gone after the unit restarts while the words are still in the recording;
// or if "Delete text" through that server does not clear it, for good.
func TestRefusedCutWarningSurvivesARestart(t *testing.T) {
	dbPath, recording, segmentID, revision := refusedCutDatabase(t)
	handler := unitServer(t, openTestStore(t, dbPath), recording)

	cut := fmt.Sprintf("/api/sessions/s/segments/%d?mode=text_and_audio&revision=%d", segmentID, revision)
	if recorder := serve(handler, http.MethodDelete, cut); recorder.Code == http.StatusNoContent {
		t.Fatal("the server accepted a cut its check refuses")
	}
	if warnings := apiWarnings(t, handler); !slices.ContainsFunc(warnings, isRefusedCutWarning) {
		t.Fatalf("status warnings after the refused cut = %q, want the refused cut's", warnings)
	}

	handler = unitServer(t, openTestStore(t, dbPath), recording) // a restart
	if warnings := apiWarnings(t, handler); !slices.ContainsFunc(warnings, isRefusedCutWarning) {
		t.Fatalf("status warnings after a restart = %q, want the refused cut's still there: the words are still in the recording", warnings)
	}

	text := fmt.Sprintf("/api/sessions/s/segments/%d?mode=text&revision=%d", segmentID, revision)
	if recorder := serve(handler, http.MethodDelete, text); recorder.Code != http.StatusNoContent {
		t.Fatalf("Delete text = %d %s", recorder.Code, recorder.Body.String())
	}
	if warnings := apiWarnings(t, handler); len(warnings) != 0 {
		t.Fatalf("status warnings after Delete text = %q, want none", warnings)
	}
	handler = unitServer(t, openTestStore(t, dbPath), recording)
	if warnings := apiWarnings(t, handler); len(warnings) != 0 {
		t.Fatalf("status warnings after Delete text and a restart = %q, want none", warnings)
	}
}
