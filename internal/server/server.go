package server

import (
	"io/fs"
	"log"
	"net/http"
	"path"
	"strings"
)

type ControlHooks struct {
	Pause           func()
	Resume          func()
	IsPaused        func() bool
	OnStatusChanged func(paused bool)
}

func Handler(staticFS fs.FS, hub *Hub, store SessionStore, controls ControlHooks) (http.Handler, error) {
	mux := http.NewServeMux()

	registerWSRoute(mux, hub)
	registerAPIRoutes(mux, store, controls)

	fileServer := http.FileServer(http.FS(staticFS))
	mux.HandleFunc("/", serveSPA(fileServer))

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

func serveSPA(fileServer http.Handler) func(http.ResponseWriter, *http.Request) {
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
		if cleanPath == "." || cleanPath == "" {
			r.URL.Path = "/"
		} else if !strings.Contains(cleanPath, ".") {
			r.URL.Path = "/index.html"
		} else {
			r.URL.Path = "/" + cleanPath
		}

		fileServer.ServeHTTP(w, r)
	}
}
