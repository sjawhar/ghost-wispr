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
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// heardTractor is a check after a cut that still hears the segment's
// "tractor" at the cut.
var heardTractor = []transcribe.TimestampedWord{{Text: "So", Start: 0.2, End: 0.5}, {Text: "tractor", Start: 62.4, End: 62.7}}

// refusingRunner makes the deletion runner main.go makes, over store, for
// refusedCutFixture's recording, with a check after each cut that hears
// heard and the refused-cut record refused.
func refusingRunner(store *storage.SQLiteStore, recording string, heard []transcribe.TimestampedWord, refused *refusedCuts) *segmentDeleteRunner {
	return withTestDefaults(&segmentDeleteRunner{
		store: store,
		ensureRecordings: func(context.Context, storage.Session) (deletionRecordingSet, error) {
			return deletionRecordingSet{Paths: []string{recording}}, nil
		},
		splice: func(context.Context, string, float64, float64) (string, error) { return "after", nil },
		transcribeCut: func(context.Context, string, string) ([]transcribe.TimestampedWord, error) {
			return heard, nil
		},
		refused: refused,
	})
}

// unitServer is the HTTP handler a unit starting on store serves, as far as
// segment deletion and /api/status go, with a check after each cut that still
// hears the segment's "tractor".
func unitServer(t *testing.T, store *storage.SQLiteStore, recording string) http.Handler {
	t.Helper()
	return unitServerHearing(t, store, recording, heardTractor)
}

// unitServerHearing is unitServer with a check after each cut that hears
// heard.
func unitServerHearing(t *testing.T, store *storage.SQLiteStore, recording string, heard []transcribe.TimestampedWord) http.Handler {
	t.Helper()
	hooks := &server.ControlHooks{}
	refused := newRefusedCuts(store)
	wireSegmentDeletion(hooks, newWarningsRegistry(), refused, refusingRunner(store, recording, heard, refused))
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
	runParallel(t)
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

// TestRefusedCutWarningClearsWhenItsSessionIsDeleted fails if a refused
// cut's status warning stays after its session is deleted: the recording and
// the words in it are gone with the session.
func TestRefusedCutWarningClearsWhenItsSessionIsDeleted(t *testing.T) {
	runParallel(t)
	dbPath, recording, segmentID, revision := refusedCutDatabase(t)
	store := openTestStore(t, dbPath)
	handler := unitServer(t, store, recording)
	cut := fmt.Sprintf("/api/sessions/s/segments/%d?mode=text_and_audio&revision=%d", segmentID, revision)
	if recorder := serve(handler, http.MethodDelete, cut); recorder.Code == http.StatusNoContent {
		t.Fatal("the server accepted a cut its check refuses")
	}
	if warnings := apiWarnings(t, handler); !slices.ContainsFunc(warnings, isRefusedCutWarning) {
		t.Fatalf("status warnings after the refused cut = %q, want the refused cut's", warnings)
	}
	if _, err := store.DeleteSession("s", t.TempDir()); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	if warnings := apiWarnings(t, handler); len(warnings) != 0 {
		t.Fatalf("status warnings after the session was deleted = %q, want none", warnings)
	}
	if cuts, err := store.RefusedCuts(); err != nil || len(cuts) != 0 {
		t.Fatalf("refused cuts kept after the session was deleted = %+v (%v), want none", cuts, err)
	}
}

// TestUncheckedCutRefusalWarnsOnTheStatusPage fails if a "Delete text and
// audio" refused because its check heard nothing at the cut gets no status
// warning, or loses it on a restart: the words are still in the recording,
// as after any refused cut.
func TestUncheckedCutRefusalWarnsOnTheStatusPage(t *testing.T) {
	runParallel(t)
	// "Then" at 64.6 s is within 0.5 s of the cut, which ends at 64.85 s, and
	// not wholly inside it, so the check must hear a word at the cut point,
	// about 62.55 s into the cut recording.
	dbPath, recording, segmentID, revision := refusedCutDatabaseThen(t, 64.6)
	deaf := []transcribe.TimestampedWord{{Text: "So", Start: 0.2, End: 0.5}, {Text: "left.", Start: 66.0, End: 66.3}}
	handler := unitServerHearing(t, openTestStore(t, dbPath), recording, deaf)
	cut := fmt.Sprintf("/api/sessions/s/segments/%d?mode=text_and_audio&revision=%d", segmentID, revision)
	if recorder := serve(handler, http.MethodDelete, cut); recorder.Code == http.StatusNoContent || !strings.Contains(recorder.Body.String(), "could not be checked") {
		t.Fatalf("delete with a deaf check = %d %s, want refused as unchecked", recorder.Code, recorder.Body.String())
	}
	if warnings := apiWarnings(t, handler); !slices.ContainsFunc(warnings, isRefusedCutWarning) {
		t.Fatalf("status warnings after a cut refused as unchecked = %q, want the refused cut's", warnings)
	}
	handler = unitServerHearing(t, openTestStore(t, dbPath), recording, deaf) // a restart
	if warnings := apiWarnings(t, handler); !slices.ContainsFunc(warnings, isRefusedCutWarning) {
		t.Fatalf("status warnings after a restart = %q, want the unchecked refusal's still there", warnings)
	}
}

// TestTouchingNeighbourCutTrimsARefusedCut fails if a successful cut of a
// touching neighbour, whose pad reaches into a refused segment, drops the
// refused cut's warning while the segment's words are still in the
// recording, instead of trimming its span.
func TestTouchingNeighbourCutTrimsARefusedCut(t *testing.T) {
	runParallel(t)
	dbPath, recording, segmentID, revision := refusedCutDatabaseThen(t, 64.6)
	store := openTestStore(t, dbPath)
	handler := unitServer(t, store, recording)
	cut := fmt.Sprintf("/api/sessions/s/segments/%d?mode=text_and_audio&revision=%d", segmentID, revision)
	if recorder := serve(handler, http.MethodDelete, cut); recorder.Code == http.StatusNoContent {
		t.Fatal("the server accepted a cut its check refuses")
	}

	// "Then left." (64.6 to 65.3 s) is cut from 64.15 to 65.75 s: its pad
	// takes the last 0.25 s of "ninety.", the refused segment's last word.
	session, err := store.GetSession("s")
	if err != nil {
		t.Fatal(err)
	}
	segments, err := store.GetSegments("s")
	if err != nil {
		t.Fatal(err)
	}
	then := segments[len(segments)-1]
	runner := refusingRunner(store, recording, []transcribe.TimestampedWord{{Text: "ninety", Start: 63.9, End: 64.1}}, newRefusedCuts(store))
	runner.splice = func(_ context.Context, path string, start, end float64) (string, error) {
		if err := os.WriteFile(path, fmt.Appendf(nil, "cut %.3f-%.3f", start, end), 0o600); err != nil {
			return "", err
		}
		return fileSHA256(path)
	}
	if err := runner.DeleteRefinedAudio(context.Background(), "s", then.ID, session.RefinedRevision); err != nil {
		t.Fatalf("clean cut of the touching neighbour %q: %v", then.Text, err)
	}
	cuts, err := store.RefusedCuts()
	if err != nil {
		t.Fatal(err)
	}
	if len(cuts) != 1 || cuts[0].Start != 63 || math.Abs(cuts[0].End-64.15) > 1e-9 {
		t.Fatalf("refused cuts after the neighbour's cut = %+v, want the refused span trimmed to 63 to 64.15 s", cuts)
	}
	if warnings := apiWarnings(t, handler); !slices.ContainsFunc(warnings, isRefusedCutWarning) {
		t.Fatalf("status warnings after the touching neighbour's cut = %q, want the refused cut's still there", warnings)
	}
}

// TestRefusedCutWarningFollowsAMerge fails if a refused cut's warning does
// not follow its segment into the session the sessions merge into, or is
// dropped when the merged session is refining or when "Delete text" of
// another segment there drops the source's refined words, while the refused
// segment's words are still in the recording.
func TestRefusedCutWarningFollowsAMerge(t *testing.T) {
	runParallel(t)
	dbPath, recording, segmentID, revision := refusedCutDatabase(t)
	store := openTestStore(t, dbPath)
	handler := unitServer(t, store, recording)
	cut := fmt.Sprintf("/api/sessions/s/segments/%d?mode=text_and_audio&revision=%d", segmentID, revision)
	if recorder := serve(handler, http.MethodDelete, cut); recorder.Code == http.StatusNoContent {
		t.Fatal("the server accepted a cut its check refuses")
	}
	inMerged := func(warning string) bool {
		return strings.Contains(warning, `"Delete text and audio" was refused`) && strings.Contains(warning, "session m:") &&
			strings.Contains(warning, "1:03–1:04") && !strings.Contains(strings.ToLower(warning), "tractor")
	}

	// Another session, with its own recording, and the merge.
	other := filepath.Join(filepath.Dir(recording), "other.mp3")
	if err := os.WriteFile(other, []byte("other"), 0o600); err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	if err := store.CreateSession("o", started); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("o", started.Add(time.Minute), other, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.MergeSessions("m", []string{"s", "o"}, started.Add(-time.Hour), started.Add(time.Minute)); err != nil {
		t.Fatalf("merge: %v", err)
	}
	if warnings := apiWarnings(t, handler); !slices.ContainsFunc(warnings, inMerged) {
		t.Fatalf("status warnings while the merged session refines = %q, want the refused cut's, naming session m", warnings)
	}

	// The merged session's refinement holds the source recording's words.
	words, err := store.RefinedWords("s", revision)
	if err != nil {
		t.Fatal(err)
	}
	generation, err := store.DeletionGeneration("m")
	if err != nil {
		t.Fatal(err)
	}
	mergedRevision, err := store.PublishRefinementAtGeneration("m", storage.RefinedTranscript(words), "nova-3", words, storage.BuildRefinedSegments(words), generation)
	if err != nil {
		t.Fatalf("publish the merged refinement: %v", err)
	}
	segmentSaying := func(text string) int64 {
		t.Helper()
		segments, err := store.GetSegments("m")
		if err != nil {
			t.Fatal(err)
		}
		for i := range segments {
			if strings.Contains(segments[i].Text, text) {
				return segments[i].ID
			}
		}
		t.Fatalf("merged segments = %+v, want one saying %q", segments, text)
		return 0
	}
	text := func(id int64, revision int) {
		t.Helper()
		target := fmt.Sprintf("/api/sessions/m/segments/%d?mode=text&revision=%d", id, revision)
		if recorder := serve(handler, http.MethodDelete, target); recorder.Code != http.StatusNoContent {
			t.Fatalf("Delete text in the merged session = %d %s", recorder.Code, recorder.Body.String())
		}
	}
	text(segmentSaying("So we met."), mergedRevision)
	if warnings := apiWarnings(t, handler); !slices.ContainsFunc(warnings, inMerged) {
		t.Fatalf("status warnings after Delete text of another segment in the merged session = %q, want the refused cut's", warnings)
	}
	handler = unitServer(t, openTestStore(t, dbPath), recording) // a restart
	if warnings := apiWarnings(t, handler); !slices.ContainsFunc(warnings, inMerged) {
		t.Fatalf("status warnings after a restart = %q, want the refused cut's", warnings)
	}

	merged, err := store.GetSession("m")
	if err != nil {
		t.Fatal(err)
	}
	text(segmentSaying("tractor"), merged.RefinedRevision)
	if warnings := apiWarnings(t, handler); len(warnings) != 0 {
		t.Fatalf("status warnings after Delete text of the refused segment = %q, want none", warnings)
	}
}
