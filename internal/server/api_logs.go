package server

import (
	"net/http"
	"strconv"
	"time"
)

func registerLogRoutes(mux *http.ServeMux, controls *ControlHooks) {
	mux.HandleFunc("GET /api/logs", func(w http.ResponseWriter, r *http.Request) {
		if controls.GetLogs == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "log viewer not configured")
			return
		}

		level := r.URL.Query().Get("level")
		limitStr := r.URL.Query().Get("limit")
		sinceStr := r.URL.Query().Get("since")

		limit := 100
		if limitStr != "" {
			if l, err := strconv.Atoi(limitStr); err == nil && l > 0 {
				limit = l
			}
		}

		var since time.Time
		if sinceStr != "" {
			if t, err := time.Parse(time.RFC3339, sinceStr); err == nil {
				since = t
			}
		}

		logs := controls.GetLogs(level, limit, since)
		writeJSON(w, http.StatusOK, logs)
	})
}
