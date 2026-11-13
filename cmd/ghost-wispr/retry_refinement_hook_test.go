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

	"google.golang.org/api/drive/v3"

	"github.com/sjawhar/ghost-wispr/internal/gdrive"
	recordingname "github.com/sjawhar/ghost-wispr/internal/recording"
	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

func TestRetryRefinementHookRestoresThenEnablesDeletion(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "retry.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	dir := t.TempDir()
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	ended := started.Add(time.Minute)
	rs := gdrive.RestoredSession{ID: "restored", StartedAt: started, EndedAt: &ended, DriveFolderID: "folder-1"}
	session := restoredDriveSession(&rs, ended)
	if err := store.ImportSession(&session, nil); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "recording.mp3")
	drive := &deletionDriveFake{
		files:     []*drive.File{{Name: recordingname.FileName(file), MimeType: "audio/mpeg"}},
		downloads: map[string][]byte{recordingname.FileName(file): []byte("audio")},
	}
	registry := newWarningsRegistry()
	refined := make(chan struct{}, 1)
	hook := newRetryRefinementHook(store, func() string { return dir }, func(ctx context.Context, current storage.Session) (deletionRecordingSet, error) {
		return restoreDeletionRecordings(ctx, store, &current, dir, drive, "")
	}, registry, func(_ context.Context, id, audioPath string, at time.Time) error {
		generation, claimed, err := store.ClaimRefinement(id, audioPath)
		if err != nil {
			return err
		}
		if !claimed {
			return context.Canceled
		}
		words := []storage.RefinedWord{{RecordingPath: audioPath, RecordingSHA256: "audio", Ordinal: 0, Text: "Restored words.", Start: 0, End: 1}}
		_, err = store.PublishRefinementAtGeneration(id, storage.RefinedTranscript(words), "test", words, storage.BuildRefinedSegments(words), generation)
		if err == nil {
			refined <- struct{}{}
		}
		return err
	})
	deleted := make(chan struct{}, 1)
	handler, err := server.Handler(fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("ok"), Mode: fs.ModePerm}}, server.NewHub(), store, &server.ControlHooks{
		RefinementRecordingAvailable: func(context.Context, string) error {
			current, err := store.GetSession("restored")
			if err != nil {
				return err
			}
			return refinementRecordingAvailable(&current, dir)
		},
		RetryRefinement: hook,
		DeleteSegmentText: func(context.Context, string, int64, int) error {
			deleted <- struct{}{}
			return nil
		},
	}, "", nil)
	if err != nil {
		t.Fatal(err)
	}

	retry := httptest.NewRecorder()
	handler.ServeHTTP(retry, httptest.NewRequest(http.MethodPost, "/api/sessions/restored/retry-refinement", nil))
	if retry.Code != http.StatusAccepted {
		t.Fatalf("retry = %d %s, want 202", retry.Code, retry.Body.String())
	}
	select {
	case <-refined:
	case <-time.After(time.Second):
		t.Fatal("retry did not restore and refine the Drive session")
	}
	current, err := store.GetSession("restored")
	if err != nil {
		t.Fatal(err)
	}
	if current.RefinedRevision == 0 || current.AudioPath != file {
		t.Fatalf("retry session = %+v, want a refined session naming %s", current, file)
	}

	deleteRequest := httptest.NewRecorder()
	handler.ServeHTTP(deleteRequest, httptest.NewRequest(http.MethodDelete, "/api/sessions/restored/segments/1?mode=text&revision=1", nil))
	if deleteRequest.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s, want 204", deleteRequest.Code, deleteRequest.Body.String())
	}
	select {
	case <-deleted:
	case <-time.After(time.Second):
		t.Fatal("delete did not reach the text deletion hook")
	}
}

func TestRetryRefinementHookWarnsWhenRestoreFails(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "warning.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	ended := started.Add(time.Minute)
	rs := gdrive.RestoredSession{ID: "restored", StartedAt: started, EndedAt: &ended, DriveFolderID: "folder-1"}
	session := restoredDriveSession(&rs, ended)
	if err := store.ImportSession(&session, nil); err != nil {
		t.Fatal(err)
	}
	registry := newWarningsRegistry()
	hook := newRetryRefinementHook(store, t.TempDir, func(context.Context, storage.Session) (deletionRecordingSet, error) {
		return deletionRecordingSet{}, errors.New("Drive listing timed out")
	}, registry, func(context.Context, string, string, time.Time) error {
		t.Fatal("refinement ran after the Drive restoration failed")
		return nil
	})
	if err := hook(context.Background(), "restored"); err == nil {
		t.Fatal("retry succeeded after the Drive restoration failed")
	}
	warnings := strings.Join(statusWarnings(t, registry), "\n")
	if !strings.Contains(warnings, "restored") || !strings.Contains(warnings, "Drive listing timed out") {
		t.Fatalf("retry warning = %q", warnings)
	}
}
