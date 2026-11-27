package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/erase"
	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// sessionWarningFamilies is how many warnings setEverySessionWarning sets for
// one session.
const sessionWarningFamilies = 6

// setEverySessionWarning sets on registry, as the app sets each, every
// warning keyed to session id that GET /api/status lists from the registry:
// a recording that does not match its Drive copy, a failed "Retry
// refinement", a refinement that could not use the deleted text's model, a
// Drive replacement owed too long, a segment deletion that needs audio
// recovery, and a refinement after a segment deletion that failed.
func setEverySessionWarning(t *testing.T, registry *warningsRegistry, id string) {
	t.Helper()
	registry.WithRecordingMismatch(func() {
		registry.Set(recordingMismatchWarningPrefix+id, fmt.Sprintf("Recording for session %s does not match its Google Drive copy; local audio is retained.", id))
	})
	failed := errors.New("batch transcription not configured")
	if err := retryRefinementWithFailureWarning(context.Background(), registry, id, func(context.Context, string) error { return failed }); !errors.Is(err, failed) {
		t.Fatalf("retry refinement of %s = %v, want %v", id, err, failed)
	}
	refinementModelWarningHook(registry)(id, "the model the deleted text was recorded under cannot be used")
	registry.Set(driveReplacementWarningKeyPrefix+id, fmt.Sprintf("Google Drive still holds the words of a deleted segment of session %s (owed for 2h0m0s). The sync sweep keeps retrying.", id))
	setPendingSpliceWarning(registry, id, id+".mp3", errors.New("ffmpeg failed"))
	registry.Set(segmentDeletionWarningKey(pendingSpliceRefinementKeyPrefix, id, ""), refinementWarningText(id, errors.New("refinement failed")))
}

// naming returns the warnings that contain id.
func naming(warnings []string, id string) []string {
	var named []string
	for _, warning := range warnings {
		if strings.Contains(warning, id) {
			named = append(named, warning)
		}
	}
	return named
}

// TestADeletedSessionLeavesNoStatusWarningNamingIt: GET /api/status lists
// warnings about one session, keyed to its id. Once DELETE /api/sessions/{id}
// has deleted a merged session, no warning names it or a session merged into
// it: not one from the registry, not the raw capture a restore could not put
// back, not a cut "Delete text and audio" refused. Another session's warnings
// stay.
func TestADeletedSessionLeavesNoStatusWarningNamingIt(t *testing.T) {
	runParallel(t)
	const (
		sourceA = "20261116140449"
		sourceB = "20261116141316"
		merged  = sourceA + "-merged"
		kept    = "20261116150000"
	)
	audioDir := t.TempDir()
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 16, 14, 4, 49, 0, time.UTC)
	for i, id := range []string{sourceA, sourceB, kept} {
		recording := filepath.Join(audioDir, id+".mp3")
		if err := os.WriteFile(recording, []byte("audio"), 0o600); err != nil {
			t.Fatal(err)
		}
		at := started.Add(time.Duration(i) * 10 * time.Minute)
		if err := store.CreateSession(id, at); err != nil {
			t.Fatal(err)
		}
		if err := store.EndSession(id, at.Add(5*time.Minute), recording, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.MergeSessions(merged, []string{sourceA, sourceB}, started, started.Add(15*time.Minute)); err != nil {
		t.Fatal(err)
	}

	registry := newWarningsRegistry()
	for _, id := range []string{merged, sourceA, sourceB, kept} {
		setEverySessionWarning(t, registry, id)
	}
	for _, id := range []string{merged, kept} {
		reservation, err := erase.Reserve(filepath.Join(audioDir, id+".pcm"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.ReserveRawSessionErase(id, reservation.OriginalPath(), reservation.HiddenPath()); err != nil {
			t.Fatal(err)
		}
		if err := store.RecordRefusedCut(storage.RefusedCut{SessionID: id, Recording: id + ".mp3", Start: 1, End: 2}); err != nil {
			t.Fatal(err)
		}
	}
	if err := refreshRawEraseReservationWarnings(store, registry); err != nil {
		t.Fatal(err)
	}

	cfgStore, _, err := config.NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	if err := cfgStore.Update(func(cfg *config.Config) { cfg.AudioDir = audioDir }); err != nil {
		t.Fatal(err)
	}
	hooks := &server.ControlHooks{
		OnSessionDeleted: sessionsDeletedHook(store, registry),
		Warnings:         apiStatusWarnings(registry, newRefusedCuts(store)),
	}
	handler, err := server.Handler(fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("ok"), Mode: fs.ModePerm}}, server.NewHub(), store, hooks, "", nil, cfgStore)
	if err != nil {
		t.Fatal(err)
	}
	status := func() []string {
		t.Helper()
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/status", nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("GET /api/status = %d %s", recorder.Code, recorder.Body.String())
		}
		var response struct {
			Warnings []string `json:"warnings"`
		}
		if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
			t.Fatal(err)
		}
		return response.Warnings
	}
	// Each session has every registry warning; the merged session and the
	// kept one also a raw capture and a refused cut.
	want := map[string]int{merged: sessionWarningFamilies + 2, sourceB: sessionWarningFamilies, kept: sessionWarningFamilies + 2}
	before := status()
	for id, n := range want {
		if got := naming(before, id); len(got) != n {
			t.Fatalf("control: before the delete %d warnings name %s, want %d: %q", len(got), id, n, got)
		}
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodDelete, "/api/sessions/"+merged, nil))
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("DELETE /api/sessions/%s = %d %s", merged, recorder.Code, recorder.Body.String())
	}
	cuts, err := store.RefusedCuts()
	if err != nil {
		t.Fatal(err)
	}
	for _, cut := range cuts {
		if cut.SessionID != kept {
			t.Errorf("the store keeps a refused cut of deleted session %s: %+v", cut.SessionID, cut)
		}
	}
	// The delete itself clears them: the registry holds none before
	// GET /api/status reads it.
	for _, id := range []string{sourceA, sourceB} {
		for _, warning := range naming(registry.List(), id) {
			t.Errorf("after the delete the registry still holds a warning naming deleted session %s: %q", id, warning)
		}
	}
	// Work that was running at the delete (a "Retry refinement" that then
	// fails) sets its warning after it; GET /api/status lists none of those.
	for _, id := range []string{merged, sourceA} {
		setEverySessionWarning(t, registry, id)
	}
	after := status()
	for _, id := range []string{sourceA, sourceB} {
		for _, warning := range naming(after, id) {
			t.Errorf("after the delete a warning names deleted session %s: %q", id, warning)
		}
	}
	if got := naming(after, kept); len(got) != want[kept] {
		t.Errorf("after the delete %d warnings name the kept session %s, want %d: %q", len(got), kept, want[kept], got)
	}
}

// TestWarningSessionReadsTheSessionOfEachKey: every warning about one
// session is read as that session's, whole, and a warning about none as no
// session's.
func TestWarningSessionReadsTheSessionOfEachKey(t *testing.T) {
	for key, want := range map[string]string{
		recordingMismatchWarningPrefix + "20261116140449-merged":                                 "20261116140449-merged",
		refinementRetryWarningKeyPrefix + "20261116140449":                                       "20261116140449",
		refinementModelWarningKeyPrefix + "20261116140449":                                       "20261116140449",
		driveReplacementWarningKeyPrefix + "20261116140449":                                      "20261116140449",
		segmentDeletionWarningKey(pendingSpliceWarningKeyPrefix, "20261116140449", "a.mp3"):      "20261116140449",
		segmentDeletionWarningKey(pendingSpliceRefinementKeyPrefix, "20261116140449-merged", ""): "20261116140449-merged",
		pendingSpliceCleanupWarningKey:                                                           "",
		pendingSpliceRecoveryWarningKey:                                                          "",
		rawEraseReservationWarningKeyPrefix + "20261116140449-/audio/.erasing-x":                 "",
		"microphone":           "",
		"recording-light":      "",
		captureStateWarningKey: "",
	} {
		if got := warningSession(key); got != want {
			t.Errorf("warningSession(%q) = %q, want %q", key, got, want)
		}
	}
}

// TestAGCAgeDeleteLeavesNoStatusWarningNamingTheSession: GC's age policy
// deletes a session for good; it leaves no warning that names it.
func TestAGCAgeDeleteLeavesNoStatusWarningNamingTheSession(t *testing.T) {
	runParallel(t)
	const id = "20261001130000"
	store, cfg := recordingMismatchSession(t, id)
	registry := newWarningsRegistry()
	setEverySessionWarning(t, registry, id)
	if got := naming(statusWarnings(t, registry), id); len(got) != sessionWarningFamilies {
		t.Fatalf("control: before the pass %d warnings name %s, want %d: %q", len(got), id, sessionWarningFamilies, got)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	hub := server.NewHub()
	connected := hub.Subscribe()
	defer hub.Unsubscribe(connected)
	if _, err := runGCPass(deletionNoticeGCStore{SQLiteStore: store, hub: hub}, &cfg, &recordingMismatchSyncer{matches: true}, &gcHealth{}, registry, logger); err != nil {
		t.Fatalf("GC pass: %v", err)
	}
	if _, err := store.GetSession(id); err == nil {
		t.Fatal("control: the GC pass kept the session, so it tests nothing")
	}
	for _, warning := range naming(statusWarnings(t, registry), id) {
		t.Errorf("after GC deleted session %s a warning names it: %q", id, warning)
	}
	// Every connected web client is told, as an API delete tells it.
	select {
	case msg := <-connected:
		if want := `"type":"session_deleted"`; !strings.Contains(string(msg), want) || !strings.Contains(string(msg), `"session_id":"`+id+`"`) {
			t.Errorf("a connected client is sent %s, want session_deleted of %s", msg, id)
		}
	case <-time.After(time.Second):
		t.Errorf("no connected client is told GC deleted session %s", id)
	}
}
