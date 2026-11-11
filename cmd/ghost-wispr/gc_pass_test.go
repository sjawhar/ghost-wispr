package main

import (
	"context"
	"errors"
	"io"
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
	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// unansweringSyncer is a Drive syncer whose Drive does not answer.
type unansweringSyncer struct{ countingSyncer }

func (*unansweringSyncer) FolderOnDrive(context.Context, string) (bool, error) {
	return false, errors.New("drive: request timed out")
}

type recordingMismatchSyncer struct {
	countingSyncer
	matches bool
}

func (s *recordingMismatchSyncer) RecordingsMatch(context.Context, string, map[string]string) (bool, error) {
	return s.matches, nil
}

// A GC pass that cannot check Drive (sync on but not running, or Drive not
// answering) is recorded with the disk use, for the status API and the web
// UI, and a hold that goes on keeps the time it began; a pass that does not
// hold back clears it.
func TestAGCPassThatCannotCheckDriveIsRecordedWithTheDiskUse(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	const id = "20261001120000"
	started := time.Now().UTC().Add(-40 * 24 * time.Hour)
	if err := store.CreateSession(id, started); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.EndSession(id, started.Add(time.Minute), ""); err != nil {
		t.Fatalf("end: %v", err)
	}
	if err := store.UpdateSummary(id, "", "A summary.", storage.SummaryCompleted, "default", "{}"); err != nil {
		t.Fatalf("summarize: %v", err)
	}
	syncTo(t, store, id, storage.SyncStateSynced, "folder-1", 0, "")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Config{GCEnabled: true, GCMaxAgeDays: 30, GDriveFolderID: "folder-1", GDriveSyncEnabled: true, AudioDir: t.TempDir()}
	health := &gcHealth{}

	if _, err := runGCPass(store, &cfg, nil, health, nil, logger); err != nil {
		t.Fatalf("gc pass: %v", err)
	}
	hold := health.Hold()
	if hold == nil {
		t.Fatal("a pass with Drive sync on and no syncer running left no hold")
	}
	if !strings.Contains(hold.Reason, "not running") || hold.DiskTotalBytes <= 0 || hold.DiskUsedBytes <= 0 || hold.DiskUsedBytes > hold.DiskTotalBytes {
		t.Errorf("hold = %+v, want why and the disk use", *hold)
	}
	began := hold.Since

	if _, err := runGCPass(store, &cfg, &unansweringSyncer{}, health, nil, logger); err != nil {
		t.Fatalf("gc pass: %v", err)
	}
	if hold = health.Hold(); hold == nil || !strings.Contains(hold.Reason, "request timed out") || !hold.Since.Equal(began) {
		t.Errorf("after a pass Drive did not answer: hold %+v, want Drive's error since %v", hold, began)
	}
	if _, err := store.GetSession(id); err != nil {
		t.Errorf("a pass that could not check Drive deleted the session: %v", err)
	}

	// The session's documents are unverified on Drive (synced by an earlier
	// version): a pass that can ask Drive clears the hold, keeps it, and
	// counts it.
	if _, err := store.DB().Exec(`UPDATE sessions SET documents_unverified = 1 WHERE id = ?`, id); err != nil {
		t.Fatalf("mark unverified: %v", err)
	}
	if _, err := runGCPass(store, &cfg, &countingSyncer{}, health, nil, logger); err != nil {
		t.Fatalf("gc pass: %v", err)
	}
	if hold = health.Hold(); hold != nil {
		t.Errorf("after a pass that could check Drive: hold %+v, want none", *hold)
	}
	if n := health.Unverified(); n != 1 {
		t.Errorf("Unverified() = %d, want 1", n)
	}
}

func TestGCPassReportsAndClearsRecordingChecksumMismatchThroughStatus(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const id = "20261001130000"
	started := time.Now().UTC().Add(-40 * 24 * time.Hour)
	audioPath := filepath.Join(t.TempDir(), "recording.mp3")
	if err := os.WriteFile(audioPath, []byte("recording"), 0o600); err != nil {
		t.Fatalf("write recording: %v", err)
	}
	if err := store.CreateSession(id, started); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.EndSession(id, started.Add(time.Minute), audioPath); err != nil {
		t.Fatalf("end: %v", err)
	}
	if err := store.UpdateSummary(id, "", "A summary.", storage.SummaryCompleted, "default", "{}"); err != nil {
		t.Fatalf("summarize: %v", err)
	}
	syncTo(t, store, id, storage.SyncStateSynced, "folder-1", 0, "")

	cfg := config.Config{GCEnabled: true, GCMaxAgeDays: 30, GDriveFolderID: "folder-1", GDriveSyncEnabled: true, AudioDir: filepath.Dir(audioPath)}
	syncer := &recordingMismatchSyncer{}
	warnings := newWarningsRegistry()
	health := &gcHealth{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	statusHandler := func() *httptest.ResponseRecorder {
		t.Helper()
		handler, handlerErr := server.Handler(
			fstest.MapFS{"index.html": {Data: []byte("ok")}},
			server.NewHub(),
			store,
			&server.ControlHooks{Warnings: warnings.List},
			"", nil,
		)
		if handlerErr != nil {
			t.Fatalf("build handler: %v", handlerErr)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/status", nil))
		return response
	}

	if _, err := runGCPass(store, &cfg, syncer, health, warnings, logger); err != nil {
		t.Fatalf("GC pass with mismatch: %v", err)
	}
	if body := statusHandler().Body.String(); !strings.Contains(body, id) || !strings.Contains(body, "does not match its Google Drive copy") {
		t.Fatalf("status warning = %s, want the mismatched recording session", body)
	}

	syncer.matches = true
	if _, err := runGCPass(store, &cfg, syncer, health, warnings, logger); err != nil {
		t.Fatalf("GC pass after checksum match: %v", err)
	}
	if body := statusHandler().Body.String(); strings.Contains(body, id) || strings.Contains(body, "does not match its Google Drive copy") {
		t.Fatalf("status warning remained after checksum matched: %s", body)
	}
}

// Each GC pass reads the current config: a config API change that turns Drive
// sync off or removes the Drive folder makes the next pass hold every
// recording, even while a syncer still runs (the settings watcher stops none
// when only the folder is removed), and undoing it lets the next pass remove
// the audio Drive verifies again.
func TestGCPassHoldsAudioWhenConfigAPIStopsDriveSync(t *testing.T) {
	for _, tc := range []struct {
		name, stop, resume string
	}{
		{"sync disabled", `{"gdrive":{"sync_enabled":false}}`, `{"gdrive":{"sync_enabled":true}}`},
		{"folder removed", `{"gdrive":{"folder_id":""}}`, `{"gdrive":{"folder_id":"folder-1"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			t.Cleanup(func() { _ = store.Close() })
			audioDir := t.TempDir()
			addRecording := func(id string) string {
				t.Helper()
				started := time.Now().UTC().Add(-time.Hour)
				path := filepath.Join(audioDir, id+".mp3")
				if err := os.WriteFile(path, make([]byte, 2*1024*1024), 0o600); err != nil {
					t.Fatalf("write recording: %v", err)
				}
				if err := store.CreateSession(id, started); err != nil {
					t.Fatalf("create session: %v", err)
				}
				if err := store.EndSession(id, started.Add(time.Minute), path); err != nil {
					t.Fatalf("end session: %v", err)
				}
				return path
			}
			unsyncedAudio := addRecording("20261101120000")
			syncedAudio := addRecording("20261101130000")
			syncTo(t, store, "20261101130000", storage.SyncStateSynced, "folder-1", 0, "")

			cfgStore, _, err := config.NewStore("")
			if err != nil {
				t.Fatalf("config store: %v", err)
			}
			if err := cfgStore.Update(func(cfg *config.Config) {
				cfg.GDriveFolderID = "folder-1"
				cfg.GDriveSyncEnabled = true
				cfg.GCEnabled = true
				cfg.GCMaxAgeDays = 0
				cfg.GCMaxAudioSizeMB = 1
				cfg.AudioDir = audioDir
			}); err != nil {
				t.Fatalf("configure GC: %v", err)
			}
			handler, err := server.Handler(fstest.MapFS{"index.html": {Data: []byte("ok")}}, server.NewHub(), store, &server.ControlHooks{}, "", nil, cfgStore)
			if err != nil {
				t.Fatalf("handler: %v", err)
			}
			patch := func(body string) {
				t.Helper()
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, httptest.NewRequest(http.MethodPatch, "/api/config", strings.NewReader(body)))
				if response.Code != http.StatusOK {
					t.Fatalf("PATCH /api/config %s: %d %s", body, response.Code, response.Body.String())
				}
			}
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			// The syncer still runs in both passes: only the config differs.
			syncer := &countingSyncer{}

			patch(tc.stop)
			stopped := cfgStore.Get()
			health := &gcHealth{}
			if _, err := runGCPass(store, &stopped, syncer, health, nil, logger); err != nil {
				t.Fatalf("GC pass after %s: %v", tc.stop, err)
			}
			for _, path := range []string{unsyncedAudio, syncedAudio} {
				if _, err := os.Stat(path); err != nil {
					t.Errorf("GC after %s removed %s: %v", tc.stop, path, err)
				}
			}
			if hold := health.Hold(); hold == nil {
				t.Errorf("GC after %s held every recording but recorded no hold", tc.stop)
			} else if hold.Reason != "Google Drive sync is off, so recordings are kept on the unit" {
				t.Errorf("GC after %s hold reason = %q", tc.stop, hold.Reason)
			}

			patch(tc.resume)
			resumed := cfgStore.Get()
			if _, err := runGCPass(store, &resumed, syncer, health, nil, logger); err != nil {
				t.Fatalf("GC pass after %s: %v", tc.resume, err)
			}
			if _, err := os.Stat(unsyncedAudio); err != nil {
				t.Errorf("GC after %s removed unsynced audio: %v", tc.resume, err)
			}
			if _, err := os.Stat(syncedAudio); !os.IsNotExist(err) {
				t.Errorf("GC after %s kept synced audio: %v", tc.resume, err)
			}
			if hold := health.Hold(); hold != nil {
				t.Errorf("GC after %s still holds: %+v", tc.resume, *hold)
			}
		})
	}
}
