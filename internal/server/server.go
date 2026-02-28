package server

import (
	"context"
	"io/fs"
	"log"
	"net/http"
	"path"
	"strings"

	"github.com/sjawhar/ghost-wispr/internal/config"
)

type ControlHooks struct {
	Pause           func()
	Resume          func()
	IsPaused        func() bool
	OnStatusChanged func(paused bool)
	Warnings        func() []string
	Presets         func() map[string]config.Preset
	Resummarize     func(ctx context.Context, sessionID, preset string) error
}

func Handler(staticFS fs.FS, hub *Hub, store SessionStore, controls ControlHooks) (http.Handler, error) {
	mux := http.NewServeMux()

	registerWSRoute(mux, hub)
	registerAPIRoutes(mux, store, controls)

	fileServer := http.FileServer(http.FS(staticFS))
	mux.HandleFunc("/", serveSPA(staticFS, fileServer))

	return mux, nil
}

func Serve(addr string, staticFS fs.FS, hub *Hub, store SessionStore, controls ControlHooks) error {
	h, err := Handler(staticFS, hub, store, controls)
	if err != nil {
		return err
	}

	log.Printf("web UI at http://%s", addr)
	return http.ListenAndServe(addr, h)
}

func serveSPA(staticFS fs.FS, fileServer http.Handler) func(http.ResponseWriter, *http.Request) {
	// Read index.html once at startup for SPA fallback
	indexHTML, err := fs.ReadFile(staticFS, "index.html")
	if err != nil {
		log.Fatalf("failed to read index.html from static assets: %v", err)
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
				log.Printf("write index.html: %v", err)
				return
			}
			return
		}

		r.URL.Path = "/" + cleanPath
		fileServer.ServeHTTP(w, r)
	}
}
