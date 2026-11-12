package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"testing/fstest"

	"github.com/sjawhar/ghost-wispr/internal/gdrive"
	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// folderBackUploader is a Drive that has every session folder again: a retry
// syncs into it.
type folderBackUploader struct{}

func (folderBackUploader) Upload(context.Context, string, []gdrive.SyncFile) (string, error) {
	return "folder-1", nil
}

func (folderBackUploader) UploadInto(context.Context, string, []gdrive.SyncFile) error { return nil }

func (folderBackUploader) FolderOnDrive(context.Context, string) (bool, error) { return true, nil }

func (folderBackUploader) RecordingsMatch(context.Context, string, map[string]string) (bool, error) {
	return true, nil
}

// A session whose Drive folder GC finds gone (deleted by hand, or a folder
// the app can no longer read in a drive that still reads) is kept on this
// unit only, with its audio, and no GC pass frees it. GET /api/status says so
// while it lasts, and stops once a retry syncs the session into its folder,
// back on Drive.
func TestStatusWarnsWhileASessionIsKeptOnThisUnitOnly(t *testing.T) {
	const id = "20261001130000"
	const warning = "1 session is kept on this unit only, because its Drive folder is gone or unreadable; retry sync once the folder is back, or, if its Drive folder was deleted on purpose, remove the session from this unit"
	store, cfg := recordingMismatchSession(t, id)
	handler, err := server.Handler(fstest.MapFS{"index.html": {Data: []byte("ok")}}, server.NewHub(), store,
		&server.ControlHooks{UnitOnlySessions: unitOnlyCounter(store)}, "", nil)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	status := func() (int, []string) {
		t.Helper()
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/status", nil))
		var resp struct {
			Warnings []string `json:"warnings"`
			UnitOnly int      `json:"unit_only_sessions"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode status %q: %v", rr.Body.String(), err)
		}
		return resp.UnitOnly, resp.Warnings
	}

	if n, warnings := status(); n != 0 || slices.Contains(warnings, warning) {
		t.Errorf("synced: unit_only_sessions %d, warnings %q; want 0 and no warning", n, warnings)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := runGCPass(store, &cfg, &folderGoneSyncer{}, &gcHealth{}, nil, logger); err != nil {
		t.Fatalf("gc pass: %v", err)
	}
	if sess, err := store.GetSession(id); err != nil || sess.SyncState != storage.SyncStateRemoteDeleted || sess.AudioPath == "" {
		t.Fatalf("after GC found the folder gone: %+v, %v; want REMOTE_DELETED with its audio", sess, err)
	}
	if n, warnings := status(); n != 1 || !slices.Contains(warnings, warning) {
		t.Errorf("kept on this unit only: unit_only_sessions %d, warnings %q; want 1 and %q", n, warnings, warning)
	}

	if err := gdrive.NewOrchestratorWithUploader(folderBackUploader{}, store, logger).SyncSession(context.Background(), id); err != nil {
		t.Fatalf("retry with the folder back: %v", err)
	}
	if n, warnings := status(); n != 0 || slices.Contains(warnings, warning) {
		t.Errorf("synced again: unit_only_sessions %d, warnings %q; want 0 and no warning", n, warnings)
	}
}
