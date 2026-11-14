package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/embedding"
	"github.com/sjawhar/ghost-wispr/internal/logging"
)

var (
	ErrDeepgramNotConfigured = errors.New("deepgram not configured")

	// ErrSyncNotConfigured is what RetrySync returns when Google Drive sync is off.
	ErrSyncNotConfigured = errors.New("google drive sync is not configured")
	// ErrDriveUnavailable is what RetrySync wraps when Google Drive did not
	// answer a question the retry needs answered first: the retry may work once
	// Drive answers.
	ErrDriveUnavailable = errors.New("google drive did not answer")
	// ErrSegmentRawAudioKept means a recoverable raw recording still holds the audio.
	ErrSegmentRawAudioKept = errors.New("a kept raw recording prevents segment deletion")
)

// The modes a segment deletion takes (DELETE
// /api/sessions/{id}/segments/{segmentID}?mode=): "Delete text" and "Delete
// text and audio".
const (
	segmentDeleteText         = "text"
	segmentDeleteTextAndAudio = "text_and_audio"
)

// KeptRawRecordings are the raw recordings kept in the audio directory because
// they could not be saved, for recovery or deletion by hand.
type KeptRawRecordings struct {
	Count     int
	Bytes     int64
	WarnBytes int64 // GET /api/status warns once Bytes exceeds this
}

// keptRawWarning tells the user in GET /api/status that recordings could not
// be saved and their raw audio is waiting in the audio directory.
func keptRawWarning(kept KeptRawRecordings) string {
	mb := float64(kept.Bytes) / (1024 * 1024)
	if kept.Count == 1 {
		return fmt.Sprintf("1 recording could not be saved: its raw audio (%.1f MB) is kept in the audio directory. Recover or delete it by hand.", mb)
	}
	return fmt.Sprintf("%d recordings could not be saved: their raw audio (%.1f MB) is kept in the audio directory. Recover or delete them by hand.", kept.Count, mb)
}

// GCHold is a GC pass that held back, deleting and removing nothing more,
// because it could not check that Drive still has the sessions (Drive sync on
// but not running, or Drive not answering), and the disk use of the audio
// directory's filesystem at that pass.
type GCHold struct {
	Reason         string
	Since          time.Time // the first pass of this hold
	DiskUsedBytes  int64
	DiskTotalBytes int64
}

// gcHoldWarning tells the user in GET /api/status that GC is on hold and the
// disk fills, so a broken sync is noticed before the disk is full.
func gcHoldWarning(hold *GCHold) string {
	const gb = 1024 * 1024 * 1024
	disk := "disk use unknown"
	if hold.DiskTotalBytes > 0 {
		disk = fmt.Sprintf("the disk is %d%% full (%.1f of %.1f GB)", hold.DiskUsedBytes*100/hold.DiskTotalBytes,
			float64(hold.DiskUsedBytes)/gb, float64(hold.DiskTotalBytes)/gb)
	}
	return fmt.Sprintf("Garbage collection is on hold since %s: %s. No session is deleted and no audio removed, so the disk fills; %s. Fix Google Drive sync.",
		hold.Since.UTC().Format("2006-01-02 15:04 UTC"), hold.Reason, disk)
}

// unverifiedWarning tells the user in GET /api/status how many older sessions
// GC keeps until a sync re-checks their documents on Drive.
func unverifiedWarning(n int) string {
	if n == 1 {
		return "1 older session is kept until its Drive documents are re-checked, the next time it is synced (when its title or summary changes)."
	}
	return fmt.Sprintf("%d older sessions are kept until their Drive documents are re-checked, each the next time it is synced (when its title or summary changes).", n)
}

// unitOnlyWarning tells the user in GET /api/status how many sessions are
// kept on this unit only, their Drive folder gone or unreadable, and how the
// warning clears.
func unitOnlyWarning(n int) string {
	if n == 1 {
		return "1 session is kept on this unit only, because its Drive folder is gone or unreadable; retry sync once the folder is back, or, if its Drive folder was deleted on purpose, remove the session from this unit"
	}
	return fmt.Sprintf("%d sessions are kept on this unit only, because their Drive folder is gone or unreadable; retry sync once the folder is back, or, if a session's Drive folder was deleted on purpose, remove the session from this unit", n)
}

type ControlHooks struct {
	// Pause stops capture. A non-nil error means capture stopped but the
	// stopped state was not persisted.
	Pause func() error
	// Resume restarts capture. A non-nil error means capture stayed paused.
	Resume            func() error
	IsPaused          func() bool
	IsCapturing       func() bool
	CaptureState      func() string
	OnStatusChanged   func(paused bool)
	Warnings          func() []string
	KeptRawRecordings func() KeptRawRecordings
	// RefreshRawEraseWarnings updates GET /api/status after a delete leaves a
	// raw erase reservation for startup recovery.
	RefreshRawEraseWarnings func()
	// GCHold is the hold of the last GC pass, nil when it did not hold back.
	GCHold func() *GCHold
	// UnverifiedSessions is how many sessions GC keeps until a sync verifies
	// their documents on Drive.
	UnverifiedSessions func() int
	// UnitOnlySessions is how many sessions are kept on this unit only:
	// REMOTE_DELETED, with their audio still here.
	UnitOnlySessions        func() int
	ActiveSession           func() (string, time.Time)
	Presets                 func() map[string]config.Preset
	Resummarize             func(ctx context.Context, sessionID, preset string) error
	OnSessionDeleted        func(sessionID string)
	OnSessionsMerged        func(sessionIDs []string)
	OnSessionMerged         func(ctx context.Context, sessionID string)
	EndSession              func(ctx context.Context) error
	StartSession            func(ctx context.Context, titleHint string) (string, error)
	StopSession             func(ctx context.Context, sessionID string) error
	TestPreset              func(ctx context.Context, presetName, sessionID string) (string, error)
	GeneratePreset          func(ctx context.Context, description string) (config.Preset, error)
	RefinePreset            func(ctx context.Context, current config.Preset, feedback string) (config.Preset, error)
	RestoreFromGDrive       func(ctx context.Context) (map[string]any, error)
	FaultDeepgramDisconnect func() error
	GetLogs                 func(level string, limit int, since time.Time) []logging.LogEntry
	// RetrySync starts a sync of the session in the background and returns
	// at once: ErrSyncNotConfigured when sync is off, or why no sync can
	// start for the session.
	RetrySync                    func(ctx context.Context, sessionID string) error
	RefinementRecordingAvailable func(ctx context.Context, sessionID string) error
	RetryRefinement              func(ctx context.Context, sessionID string) error
	RepairSummaries              func(ctx context.Context) (int, error)
	DiagnoseMic                  func(ctx context.Context) (map[string]any, error)
	// DeleteSegment is "Delete text and audio" (mode=text_and_audio) for a
	// specific batch transcript revision.
	DeleteSegment func(ctx context.Context, sessionID string, segmentID int64, revision int) error
	// DeleteSegmentText is "Delete text" (mode=text) for a specific batch
	// transcript revision.
	DeleteSegmentText   func(ctx context.Context, sessionID string, segmentID int64, revision int) error
	BatchBackfill       func(ctx context.Context) (batchName string, sessionCount int, err error)
	BatchPoll           func(ctx context.Context, batchName string) (map[string]any, error)
	TTSSpeak            func(text, voice, provider string) (string, error)
	TTSStatus           func(id string) (map[string]any, bool)
	TTSWaitForSynthesis func(ctx context.Context, id string) (map[string]any, error)
	TTSHasProvider      func() bool
	TTSHasSpeaker       func() bool
}

func Handler(staticFS fs.FS, hub *Hub, store SessionStore, controls *ControlHooks, authToken string, healthChecker HealthChecker, cfgStore ...*config.Store) (http.Handler, error) {
	return HandlerWithLogger(staticFS, hub, store, controls, authToken, healthChecker, slog.Default(), nil, cfgStore...)
}

func HandlerWithLogger(staticFS fs.FS, hub *Hub, store SessionStore, controls *ControlHooks, authToken string, healthChecker HealthChecker, logger *slog.Logger, embeddingClient embedding.Client, cfgStore ...*config.Store) (http.Handler, error) {
	moduleLogger := logging.WithModule(logger, "server")
	if healthChecker == nil {
		healthChecker = &DefaultHealthChecker{}
	}
	mux := http.NewServeMux()

	registerWSRoute(mux, hub, moduleLogger)
	var cs *config.Store
	if len(cfgStore) > 0 {
		cs = cfgStore[0]
	}
	registerAPIRoutes(mux, store, controls, healthChecker, cs, embeddingClient)

	fileServer := http.FileServer(http.FS(staticFS))
	mux.HandleFunc("/", serveSPA(staticFS, fileServer, moduleLogger))

	withAuth := BasicAuthMiddleware(authToken)(mux)
	withRequestID := logging.RequestIDMiddleware(moduleLogger, nil)(noStoreAPI(withAuth))
	return withRequestID, nil
}

// noStoreAPI keeps every API response out of the browser's caches
// (Cache-Control: no-store): transcripts, summaries and recordings. A segment
// deleted on the unit must not stay in a browser's disk cache, and a spliced
// recording is served at the URL the unspliced one had.
func noStoreAPI(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

func Serve(addr string, staticFS fs.FS, hub *Hub, store SessionStore, controls *ControlHooks, authToken string, healthChecker HealthChecker, cfgStore ...*config.Store) error {
	logger := logging.WithModule(slog.Default(), "server")
	h, err := HandlerWithLogger(staticFS, hub, store, controls, authToken, healthChecker, logger, nil, cfgStore...)
	if err != nil {
		return err
	}

	logger.Info("web UI available", "operation", "serve_http", "address", addr)
	return http.ListenAndServe(addr, h)
}

func serveSPA(staticFS fs.FS, fileServer http.Handler, logger *slog.Logger) func(http.ResponseWriter, *http.Request) {
	moduleLogger := logging.WithModule(logger, "server")
	// Read index.html once at startup for SPA fallback
	indexHTML, err := fs.ReadFile(staticFS, "index.html")
	if err != nil {
		moduleLogger.Error("failed to read index.html from static assets", "operation", "load_static_index", "error", err)
		panic(err)
	}

	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/ws" {
			http.NotFound(w, r)
			return
		}

		if r.URL.Path == "/manifest.json" || r.URL.Path == "/manifest.webmanifest" {
			w.Header().Set("Content-Type", "application/manifest+json")
		}
		if r.URL.Path == "/sw.js" {
			w.Header().Set("Service-Worker-Allowed", "/")
			w.Header().Set("Cache-Control", "no-cache")
		}

		cleanPath := path.Clean(strings.TrimPrefix(r.URL.Path, "/"))
		if cleanPath == "." || cleanPath == "" || !strings.Contains(cleanPath, ".") {
			// SPA route: serve index.html directly (avoids FileServer redirect loop)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if _, err := w.Write(indexHTML); err != nil {
				logging.FromContext(r.Context(), moduleLogger).Warn("failed to write index.html", "operation", "write_index", "error", err)
				return
			}
			return
		}

		r.URL.Path = "/" + cleanPath
		fileServer.ServeHTTP(w, r)
	}
}
