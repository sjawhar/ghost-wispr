package main

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/gdrive"
	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

func TestDriveRestoredSessionCanRetryBeforeDeletion(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "restore.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	ended := started.Add(time.Minute)
	rs := gdrive.RestoredSession{
		ID:            "restored",
		Title:         "Restored conversation",
		StartedAt:     started,
		EndedAt:       &ended,
		DriveFolderID: "folder-1",
	}
	session := restoredDriveSession(&rs, ended)
	if err := store.ImportSession(&session, nil); err != nil {
		t.Fatal(err)
	}
	stored, err := store.GetSession("restored")
	if err != nil {
		t.Fatal(err)
	}
	if stored.RefinementStatus != storage.RefinementNoAudio || stored.AudioRemovedAt == "" {
		t.Fatalf("restored session = %+v, want no_audio with an audio removal marker", stored)
	}
	if err := refinementRecordingAvailable(&stored, t.TempDir()); err != nil {
		t.Fatalf("Drive-restored session is not retryable: %v", err)
	}

	retried := make(chan string, 2)
	handler, err := server.Handler(fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("ok"), Mode: fs.ModePerm}}, server.NewHub(), store, &server.ControlHooks{
		RefinementRecordingAvailable: func(context.Context, string) error {
			current, err := store.GetSession("restored")
			if err != nil {
				return err
			}
			return refinementRecordingAvailable(&current, t.TempDir())
		},
		RetryRefinement: func(_ context.Context, id string) error {
			retried <- id
			return nil
		},
	}, "", nil)
	if err != nil {
		t.Fatal(err)
	}

	retry := httptest.NewRecorder()
	handler.ServeHTTP(retry, httptest.NewRequest(http.MethodPost, "/api/sessions/restored/retry-refinement", nil))
	if retry.Code != http.StatusAccepted {
		t.Fatalf("retry restored session = %d %s, want 202", retry.Code, retry.Body.String())
	}
	if got := <-retried; got != "restored" {
		t.Fatalf("retry session = %q, want restored", got)
	}

	deleteRequest := httptest.NewRecorder()
	handler.ServeHTTP(deleteRequest, httptest.NewRequest(http.MethodDelete, "/api/sessions/restored/segments/1?mode=text&revision=1", nil))
	if deleteRequest.Code != http.StatusConflict || !strings.Contains(deleteRequest.Body.String(), "restoring it from Drive") {
		t.Fatalf("delete restored session = %d %s, want the Drive retry conflict", deleteRequest.Code, deleteRequest.Body.String())
	}
	if got := <-retried; got != "restored" {
		t.Fatalf("delete retry session = %q, want restored", got)
	}
}

func TestDeleteNoAudioDoesNotPromiseUnavailableDriveRestore(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "unavailable.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	if err := store.CreateSession("unavailable", started); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("unavailable", started.Add(time.Minute), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET refinement_status = 'no_audio' WHERE id = 'unavailable'`); err != nil {
		t.Fatal(err)
	}
	retried := make(chan string, 1)
	handler, err := server.Handler(fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("ok"), Mode: fs.ModePerm}}, server.NewHub(), store, &server.ControlHooks{
		RefinementRecordingAvailable: func(context.Context, string) error {
			return errors.New("no recording is available on this unit or Drive")
		},
		RetryRefinement: func(_ context.Context, id string) error {
			retried <- id
			return nil
		},
	}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodDelete, "/api/sessions/unavailable/segments/1?mode=text&revision=1", nil))
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "no recording is available") {
		t.Fatalf("delete unavailable no-audio session = %d %s, want true recording availability error", response.Code, response.Body.String())
	}
	select {
	case id := <-retried:
		t.Fatalf("delete started impossible refinement retry for %s", id)
	case <-time.After(50 * time.Millisecond):
	}
}
