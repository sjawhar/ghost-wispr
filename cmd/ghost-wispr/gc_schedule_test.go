package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// statusGCHold returns the gc_hold reason GET /api/status reports with
// health's hold, "" when it reports none.
func statusGCHold(t *testing.T, health *gcHealth) string {
	t.Helper()
	handler, err := server.Handler(fstest.MapFS{"index.html": {Data: []byte("ok")}}, server.NewHub(), nil, &server.ControlHooks{GCHold: health.Hold}, "", nil)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("GET /api/status: %d %s", response.Code, response.Body.String())
	}
	var status struct {
		GCHold *struct {
			Reason string `json:"reason"`
		} `json:"gc_hold"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode status: %v (body=%s)", err, response.Body.String())
	}
	if status.GCHold == nil {
		return ""
	}
	return status.GCHold.Reason
}

// TestGCReportsItsHoldAsItStarts fails if the app, starting again with Drive
// sync off, waits for the first hourly GC pass before GET /api/status says
// GC keeps the recordings on the unit.
func TestGCReportsItsHoldAsItStarts(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfgStore, _, err := config.NewStore("")
	if err != nil {
		t.Fatalf("config store: %v", err)
	}
	audioDir := t.TempDir()
	if err := cfgStore.Update(func(cfg *config.Config) {
		cfg.GCEnabled = true
		cfg.GDriveSyncEnabled = false
		cfg.AudioDir = audioDir
	}); err != nil {
		t.Fatalf("configure GC: %v", err)
	}
	health := &gcHealth{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if got := statusGCHold(t, health); got != "" {
		t.Fatalf("gc_hold before GC started = %q, want none", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	collected := goDone(func() {
		collectGarbage(ctx, cfgStore, store, &driveSync{}, health, newWarningsRegistry(), logger, time.Hour)
	})
	t.Cleanup(func() {
		cancel()
		if !returnedWithin(collected, testWait) {
			t.Error("GC still running after its context ended")
		}
	})

	const want = "Google Drive sync is off, so recordings are kept on the unit"
	deadline := time.Now().Add(testWait)
	got := statusGCHold(t, health)
	for got != want && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
		got = statusGCHold(t, health)
	}
	if got != want {
		t.Fatalf("gc_hold %v after GC started, with its first tick an hour away = %q, want %q", testWait, got, want)
	}
}
