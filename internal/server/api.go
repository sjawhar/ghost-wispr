package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

var sessionIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

type SessionStore interface {
	GetSessionsByDate(date string) ([]storage.Session, error)
	GetSession(id string) (storage.Session, error)
	GetSegments(sessionID string) ([]transcribe.Segment, error)
	GetDates() ([]string, error)
}

func registerAPIRoutes(mux *http.ServeMux, store SessionStore, controls ControlHooks) {
	mux.HandleFunc("GET /api/sessions", func(w http.ResponseWriter, r *http.Request) {
		date := r.URL.Query().Get("date")
		if date == "" {
			date = time.Now().UTC().Format("2006-01-02")
		}

		sessions, err := store.GetSessionsByDate(date)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("list sessions: %v", err))
			return
		}

		writeJSON(w, http.StatusOK, sessions)
	})

	mux.HandleFunc("GET /api/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		sessionID := r.PathValue("id")
		if !validSessionID(sessionID) {
			writeJSONError(w, http.StatusForbidden, "invalid session id")
			return
		}

		sessionData, err := store.GetSession(sessionID)
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, sql.ErrNoRows) {
				status = http.StatusNotFound
			}
			writeJSONError(w, status, fmt.Sprintf("get session: %v", err))
			return
		}

		segments, err := store.GetSegments(sessionID)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("get session segments: %v", err))
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"session":  sessionData,
			"segments": segments,
		})
	})

	mux.HandleFunc("GET /api/sessions/{id}/audio", func(w http.ResponseWriter, r *http.Request) {
		sessionID := r.PathValue("id")
		if !validSessionID(sessionID) {
			writeJSONError(w, http.StatusForbidden, "invalid session id")
			return
		}

		sessionData, err := store.GetSession(sessionID)
		if err != nil {
			writeJSONError(w, http.StatusNotFound, "session not found")
			return
		}

		if sessionData.AudioPath == "" {
			writeJSONError(w, http.StatusNotFound, "audio not available")
			return
		}

		cleanPath := filepath.Clean(sessionData.AudioPath)
		if cleanPath == "" || cleanPath == "." || cleanPath == ".." || strings.Contains(cleanPath, "..") {
			writeJSONError(w, http.StatusForbidden, "invalid audio path")
			return
		}

		f, err := os.Open(cleanPath)
		if err != nil {
			writeJSONError(w, http.StatusNotFound, "audio file not found")
			return
		}
		defer func() { _ = f.Close() }()

		info, err := f.Stat()
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("stat audio: %v", err))
			return
		}

		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		w.Header().Set("Content-Type", contentTypeForAudio(cleanPath))
		http.ServeContent(w, r, filepath.Base(cleanPath), info.ModTime(), f)
	})

	mux.HandleFunc("GET /api/dates", func(w http.ResponseWriter, r *http.Request) {
		dates, err := store.GetDates()
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("get dates: %v", err))
			return
		}
		writeJSON(w, http.StatusOK, dates)
	})

	mux.HandleFunc("POST /api/pause", func(w http.ResponseWriter, r *http.Request) {
		if controls.Pause != nil {
			controls.Pause()
		}
		if controls.OnStatusChanged != nil {
			controls.OnStatusChanged(true)
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("POST /api/resume", func(w http.ResponseWriter, r *http.Request) {
		if controls.Resume != nil {
			controls.Resume()
		}
		if controls.OnStatusChanged != nil {
			controls.OnStatusChanged(false)
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		paused := false
		if controls.IsPaused != nil {
			paused = controls.IsPaused()
		}
		var warnings []string
		if controls.Warnings != nil {
			warnings = controls.Warnings()
		}
		if warnings == nil {
			warnings = []string{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"paused": paused, "warnings": warnings})
	})
}

func validSessionID(id string) bool {
	return sessionIDPattern.MatchString(id)
}

func contentTypeForAudio(path string) string {
	ext := filepath.Ext(path)
	switch ext {
	case ".mp3":
		return "audio/mpeg"
	case ".wav":
		return "audio/wav"
	default:
		return "application/octet-stream"
	}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
