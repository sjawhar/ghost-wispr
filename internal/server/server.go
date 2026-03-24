package server

import (
	"context"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/logging"
)

type ControlHooks struct {
	Pause             func()
	Resume            func()
	IsPaused          func() bool
	OnStatusChanged   func(paused bool)
	Warnings          func() []string
	ActiveSession     func() (string, time.Time)
	Presets           func() map[string]config.Preset
	Resummarize       func(ctx context.Context, sessionID, preset string) error
	OnSessionMerged   func(ctx context.Context, sessionID string)
	EndSession        func(ctx context.Context) error
	StartSession      func(ctx context.Context, titleHint string) (string, error)
	StopSession       func(ctx context.Context, sessionID string) error
	TestPreset        func(ctx context.Context, presetName, sessionID string) (string, error)
	GeneratePreset    func(ctx context.Context, description string) (config.Preset, error)
	RefinePreset      func(ctx context.Context, current config.Preset, feedback string) (config.Preset, error)
	RestoreFromGDrive func(ctx context.Context) (map[string]any, error)
	FaultDeepgramDisconnect func() error
	GetLogs           func(level string, limit int, since time.Time) []logging.LogEntry
	RetrySync         func(ctx context.Context, sessionID string) error
	RetryRefinement   func(ctx context.Context, sessionID string) error
}

func Handler(staticFS fs.FS, hub *Hub, store SessionStore, controls *ControlHooks, authToken string, healthChecker HealthChecker, cfgStore ...*config.Store) (http.Handler, error) {
	return HandlerWithLogger(staticFS, hub, store, controls, authToken, healthChecker, slog.Default(), cfgStore...)
}

func HandlerWithLogger(staticFS fs.FS, hub *Hub, store SessionStore, controls *ControlHooks, authToken string, healthChecker HealthChecker, logger *slog.Logger, cfgStore ...*config.Store) (http.Handler, error) {
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
	registerAPIRoutes(mux, store, controls, healthChecker, cs)

	fileServer := http.FileServer(http.FS(staticFS))
	mux.HandleFunc("/", serveSPA(staticFS, fileServer, moduleLogger))

	withAuth := BasicAuthMiddleware(authToken)(mux)
	withRequestID := logging.RequestIDMiddleware(moduleLogger, nil)(withAuth)
	return withRequestID, nil
}

func Serve(addr string, staticFS fs.FS, hub *Hub, store SessionStore, controls *ControlHooks, authToken string, healthChecker HealthChecker, cfgStore ...*config.Store) error {
	logger := logging.WithModule(slog.Default(), "server")
	h, err := HandlerWithLogger(staticFS, hub, store, controls, authToken, healthChecker, logger, cfgStore...)
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
