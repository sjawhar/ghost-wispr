package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/session"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

var sessionIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

type SessionStore interface {
	GetSessionsByDate(date string, includeDiscarded bool) ([]storage.Session, error)
	GetSession(id string) (storage.Session, error)
	GetSegments(sessionID string) ([]transcribe.Segment, error)
	GetDates() ([]string, error)
	UpdateTitle(sessionID, title string) error
	DeleteSession(id string) error
	MergeSessions(newID string, sourceIDs []string, startedAt, endedAt time.Time) error
}

// VersionInfo holds build metadata exposed via /api/version.
type VersionInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildTime string `json:"build_time"`
}

var versionInfo VersionInfo

// SetVersionInfo sets the build metadata for the /api/version endpoint.
func SetVersionInfo(v VersionInfo) { versionInfo = v }

func registerAPIRoutes(mux *http.ServeMux, store SessionStore, controls *ControlHooks, healthChecker HealthChecker, cfgStore *config.Store) {

	// Health check endpoints
	mux.HandleFunc("GET /healthz/live", func(w http.ResponseWriter, r *http.Request) {
		handleHealthzLive(w, r, healthChecker)
	})
	mux.HandleFunc("GET /healthz/ready", func(w http.ResponseWriter, r *http.Request) {
		handleHealthzReady(w, r, healthChecker)
	})

	// Fault injection endpoint — only available in test mode
	mux.HandleFunc("POST /api/test/fault/deepgram-disconnect", func(w http.ResponseWriter, r *http.Request) {
		if os.Getenv("GHOST_WISPR_TEST_MODE") != "true" {
			writeJSONError(w, http.StatusForbidden, "test mode not enabled")
			return
		}
		if controls.FaultDeepgramDisconnect == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "deepgram not configured")
			return
		}
		if err := controls.FaultDeepgramDisconnect(); err != nil {
			writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("fault injection failed: %v", err))
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"triggered": true})
	})

	mux.HandleFunc("GET /api/version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, versionInfo)
	})
	registerRestoreRoutes(mux, controls)

	mux.HandleFunc("GET /api/sessions", func(w http.ResponseWriter, r *http.Request) {
		date := r.URL.Query().Get("date")
		if date == "" {
			date = time.Now().UTC().Format("2006-01-02")
		}

		sessions, err := store.GetSessionsByDate(date, r.URL.Query().Get("include_discarded") == "true")
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

	mux.HandleFunc("PATCH /api/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		sessionID := r.PathValue("id")
		if !validSessionID(sessionID) {
			writeJSONError(w, http.StatusForbidden, "invalid session id")
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		var body struct {
			Title *string `json:"title"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
			writeJSONError(w, http.StatusBadRequest, "invalid request body")
			return
		}

		if body.Title != nil && len(*body.Title) > 500 {
			writeJSONError(w, http.StatusBadRequest, "title too long (max 500 characters)")
			return
		}

		if body.Title != nil {
			if err := store.UpdateTitle(sessionID, *body.Title); err != nil {
				status := http.StatusInternalServerError
				if errors.Is(err, os.ErrNotExist) || errors.Is(err, sql.ErrNoRows) {
					status = http.StatusNotFound
				}
				writeJSONError(w, status, fmt.Sprintf("update session title: %v", err))
				return
			}
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

		writeJSON(w, http.StatusOK, sessionData)
	})

	mux.HandleFunc("DELETE /api/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
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
		if sessionData.Status == "active" {
			writeJSONError(w, http.StatusConflict, "cannot delete active session")
			return
		}

		if err := store.DeleteSession(sessionID); err != nil {
			writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("delete session: %v", err))
			return
		}

		// Clean up audio files after successful DB deletion.
		if sessionData.AudioPath != "" {
			for _, p := range strings.Split(sessionData.AudioPath, ",") {
				p = strings.TrimSpace(p)
				if p != "" {
					_ = os.Remove(p)
				}
			}
		}

		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("POST /api/sessions/merge", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		var body struct {
			SessionIDs []string `json:"session_ids"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if len(body.SessionIDs) < 2 {
			writeJSONError(w, http.StatusBadRequest, "at least 2 session IDs required")
			return
		}

		var earliest time.Time
		var latest time.Time
		for _, id := range body.SessionIDs {
			if !validSessionID(id) {
				writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("invalid session id: %s", id))
				return
			}

			sess, err := store.GetSession(id)
			if err != nil {
				writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("session not found: %s", id))
				return
			}

			if earliest.IsZero() || sess.StartedAt.Before(earliest) {
				earliest = sess.StartedAt
			}
			if sess.EndedAt != nil && (latest.IsZero() || sess.EndedAt.After(latest)) {
				latest = *sess.EndedAt
			}
		}

		newID := earliest.UTC().Format("20060102150405") + "-merged"
		if err := store.MergeSessions(newID, body.SessionIDs, earliest, latest); err != nil {
			writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("merge sessions: %v", err))
			return
		}

		if controls.OnSessionMerged != nil {
			go controls.OnSessionMerged(context.Background(), newID)
		}

		merged, err := store.GetSession(newID)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("get merged session: %v", err))
			return
		}

		writeJSON(w, http.StatusOK, merged)
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
		if filepath.IsAbs(cleanPath) {
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
		if dates == nil {
			dates = []string{}
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

	mux.HandleFunc("POST /api/session/end", func(w http.ResponseWriter, r *http.Request) {
		if controls.EndSession == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "session management not available")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := controls.EndSession(ctx); err != nil {
			if errors.Is(err, session.ErrNoActiveSession) {
				writeJSONError(w, http.StatusConflict, "no active session")
			} else {
				writeJSONError(w, http.StatusInternalServerError, "internal error")
			}
			return
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
		var activeSessionID string
		var activeSessionStartedAt string
		if controls.ActiveSession != nil {
			id, startedAt := controls.ActiveSession()
			activeSessionID = id
			if id != "" {
				activeSessionStartedAt = startedAt.UTC().Format(time.RFC3339Nano)
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"paused":                    paused,
			"warnings":                  warnings,
			"active_session_id":         activeSessionID,
			"active_session_started_at": activeSessionStartedAt,
		})
	})

	mux.HandleFunc("GET /api/presets", func(w http.ResponseWriter, r *http.Request) {
		if controls.Presets == nil {
			writeJSON(w, http.StatusOK, map[string]any{})
			return
		}
		presets := controls.Presets()
		result := make(map[string]string, len(presets))
		for name, p := range presets {
			result[name] = p.Description
		}
		writeJSON(w, http.StatusOK, result)
	})

	mux.HandleFunc("POST /api/sessions/{id}/resummarize", func(w http.ResponseWriter, r *http.Request) {
		sessionID := r.PathValue("id")
		if !validSessionID(sessionID) {
			writeJSONError(w, http.StatusForbidden, "invalid session id")
			return
		}

		var body struct {
			Preset string `json:"preset"`
		}
		if r.Body != nil {
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
				writeJSONError(w, http.StatusBadRequest, "invalid request body")
				return
			}
		}

		if controls.Resummarize == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "summarization not configured")
			return
		}

		go func() {
			_ = controls.Resummarize(context.Background(), sessionID, body.Preset)
		}()

		w.WriteHeader(http.StatusAccepted)
	})

	// Config endpoints — only registered if cfgStore is provided.
	if cfgStore != nil {
		mux.HandleFunc("GET /api/config", handleGetConfig(cfgStore))
		mux.HandleFunc("PATCH /api/config", handlePatchConfig(cfgStore))
		if controls.TestPreset != nil {
			mux.HandleFunc("POST /api/config/presets/{name}/test", handleTestPreset(cfgStore, controls.TestPreset))
		}
		if controls.GeneratePreset != nil {
			mux.HandleFunc("POST /api/config/presets/generate", handleGeneratePreset(controls.GeneratePreset))
		}
		if controls.RefinePreset != nil {
			mux.HandleFunc("POST /api/config/presets/refine", handleRefinePreset(cfgStore, controls.RefinePreset))
		}
	}
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

// configResponse is the shape returned by GET /api/config.
type configResponse struct {
	SilenceTimeout     string                      `json:"silence_timeout"`
	MinSessionSegments int                         `json:"min_session_segments"`
	Summarization      configSummarizationResponse `json:"summarization"`
	Transcription      configTranscriptionResponse `json:"transcription"`
	GDrive             configGDriveResponse        `json:"gdrive"`
	GC                 configGCResponse            `json:"gc"`
	APIKeys            map[string]bool             `json:"api_keys"`
}

type configSummarizationResponse struct {
	Model   string                   `json:"model"`
	BaseURL string                   `json:"base_url"`
	Presets map[string]config.Preset `json:"presets"`
}

type configTranscriptionResponse struct {
	Endpointing    string `json:"endpointing"`
	UtteranceEndMs string `json:"utterance_end_ms"`
}

type configGDriveResponse struct {
	FolderID       string `json:"folder_id"`
	HasCredentials bool   `json:"has_credentials"`
	SyncEnabled    bool   `json:"sync_enabled"`
}

type configGCResponse struct {
	Enabled        bool `json:"enabled"`
	MaxAgeDays     int  `json:"max_age_days"`
	MaxAudioSizeMB int  `json:"max_audio_size_mb"`
}

func handleGetConfig(cfgStore *config.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := cfgStore.Get()
		resp := configResponse{
			SilenceTimeout:     cfg.SilenceTimeout,
			MinSessionSegments: cfg.MinSessionSegments,
			Summarization: configSummarizationResponse{
				Model:   cfg.Summarization.Model,
				BaseURL: cfg.Summarization.BaseURL,
				Presets: cfg.Summarization.Presets,
			},
			Transcription: configTranscriptionResponse{
				Endpointing:    cfg.Transcription.Endpointing,
				UtteranceEndMs: cfg.Transcription.UtteranceEndMs,
			},
			GDrive: configGDriveResponse{
				FolderID:       cfg.GDriveFolderID,
				HasCredentials: cfg.GoogleCredentialsFile != "" && fileExists(cfg.GoogleCredentialsFile),
				SyncEnabled:    cfg.GDriveSyncEnabled,
			},
			GC: configGCResponse{
				Enabled:        cfg.GCEnabled,
				MaxAgeDays:     cfg.GCMaxAgeDays,
				MaxAudioSizeMB: cfg.GCMaxAudioSizeMB,
			},
			APIKeys: map[string]bool{
				"deepgram":  cfg.DeepgramAPIKey != "",
				"openai":    cfg.OpenAIAPIKey != "",
				"anthropic": cfg.AnthropicAPIKey != "",
				"gemini":    cfg.GeminiAPIKey != "",
			},
		}
		if resp.Summarization.Presets == nil {
			resp.Summarization.Presets = map[string]config.Preset{}
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// configPatch represents the JSON merge-patch body for PATCH /api/config.
type configPatch struct {
	SilenceTimeout     *string             `json:"silence_timeout,omitempty"`
	MinSessionSegments *int                `json:"min_session_segments,omitempty"`
	Summarization      *summarizationPatch `json:"summarization,omitempty"`
	Transcription      *transcriptionPatch `json:"transcription,omitempty"`
	APIKeys            map[string]string   `json:"api_keys,omitempty"`
	GDrive             *gdrivePatch        `json:"gdrive,omitempty"`
	GC                 *gcPatch            `json:"gc,omitempty"`
}

type summarizationPatch struct {
	Model   *string                   `json:"model,omitempty"`
	BaseURL *string                   `json:"base_url,omitempty"`
	Presets map[string]*config.Preset `json:"presets,omitempty"` // null value = delete
}

type transcriptionPatch struct {
	Endpointing    *string `json:"endpointing,omitempty"`
	UtteranceEndMs *string `json:"utterance_end_ms,omitempty"`
}

type gdrivePatch struct {
	FolderID          *string `json:"folder_id,omitempty"`
	CredentialsBase64 *string `json:"credentials_base64,omitempty"`
	SyncEnabled       *bool   `json:"sync_enabled,omitempty"`
}

type gcPatch struct {
	Enabled        *bool `json:"enabled,omitempty"`
	MaxAgeDays     *int  `json:"max_age_days,omitempty"`
	MaxAudioSizeMB *int  `json:"max_audio_size_mb,omitempty"`
}

func handlePatchConfig(cfgStore *config.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		var patch configPatch
		if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
			writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
			return
		}

		var patchErr error
		err := cfgStore.Update(func(c *config.Config) {
			patchErr = applyConfigPatch(c, &patch)
		})
		if patchErr != nil {
			writeJSONError(w, http.StatusBadRequest, patchErr.Error())
			return
		}
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}

		// Return updated config.
		handleGetConfig(cfgStore)(w, r)
	}
}

func applyConfigPatch(c *config.Config, p *configPatch) error {
	if p.SilenceTimeout != nil {
		c.SilenceTimeout = *p.SilenceTimeout
	}
	if p.MinSessionSegments != nil {
		if *p.MinSessionSegments < 0 {
			return fmt.Errorf("min_session_segments must be non-negative")
		}
		c.MinSessionSegments = *p.MinSessionSegments
	}

	if p.Summarization != nil {
		if p.Summarization.Model != nil {
			c.Summarization.Model = *p.Summarization.Model
		}
		if p.Summarization.BaseURL != nil {
			c.Summarization.BaseURL = *p.Summarization.BaseURL
		}
		if p.Summarization.Presets != nil {
			if c.Summarization.Presets == nil {
				c.Summarization.Presets = make(map[string]config.Preset)
			}
			for name, preset := range p.Summarization.Presets {
				if preset == nil {
					// null = delete per RFC 7386
					delete(c.Summarization.Presets, name)
				} else {
					// Merge: only overwrite non-zero fields.
					existing := c.Summarization.Presets[name]
					if preset.Description != "" {
						existing.Description = preset.Description
					}
					if preset.SystemPrompt != "" {
						existing.SystemPrompt = preset.SystemPrompt
					}
					if preset.UserTemplate != "" {
						existing.UserTemplate = preset.UserTemplate
					}
					if preset.Model != "" {
						existing.Model = preset.Model
					}
					c.Summarization.Presets[name] = existing
				}
			}
		}
	}

	if p.Transcription != nil {
		if p.Transcription.Endpointing != nil {
			c.Transcription.Endpointing = *p.Transcription.Endpointing
		}
		if p.Transcription.UtteranceEndMs != nil {
			c.Transcription.UtteranceEndMs = *p.Transcription.UtteranceEndMs
		}
	}

	if p.APIKeys != nil {
		for provider, key := range p.APIKeys {
			switch provider {
			case "deepgram":
				c.DeepgramAPIKey = key
			case "openai":
				c.OpenAIAPIKey = key
			case "anthropic":
				c.AnthropicAPIKey = key
			case "gemini":
				c.GeminiAPIKey = key
			default:
				return fmt.Errorf("unknown API key provider %q", provider)
			}
		}
	}

	if p.GDrive != nil {
		if p.GDrive.FolderID != nil {
			c.GDriveFolderID = *p.GDrive.FolderID
		}
		if p.GDrive.SyncEnabled != nil {
			c.GDriveSyncEnabled = *p.GDrive.SyncEnabled
		}
		// credentials_base64 handling deferred to GDrive integration task
	}

	if p.GC != nil {
		if p.GC.Enabled != nil {
			c.GCEnabled = *p.GC.Enabled
		}
		if p.GC.MaxAgeDays != nil {
			if *p.GC.MaxAgeDays <= 0 {
				return fmt.Errorf("gc.max_age_days must be positive")
			}
			c.GCMaxAgeDays = *p.GC.MaxAgeDays
		}
		if p.GC.MaxAudioSizeMB != nil {
			if *p.GC.MaxAudioSizeMB <= 0 {
				return fmt.Errorf("gc.max_audio_size_mb must be positive")
			}
			c.GCMaxAudioSizeMB = *p.GC.MaxAudioSizeMB
		}
	}
	return nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func handleTestPreset(cfgStore *config.Store, testFn func(ctx context.Context, presetName, sessionID string) (string, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		presetName := r.PathValue("name")
		if presetName == "" {
			writeJSONError(w, http.StatusBadRequest, "preset name is required")
			return
		}

		// Verify preset exists.
		cfg := cfgStore.Get()
		if _, ok := cfg.Summarization.Presets[presetName]; !ok {
			writeJSONError(w, http.StatusNotFound, fmt.Sprintf("preset %q not found", presetName))
			return
		}

		var body struct {
			SessionID string `json:"session_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if body.SessionID == "" {
			writeJSONError(w, http.StatusBadRequest, "session_id is required")
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()

		summary, err := testFn(ctx, presetName, body.SessionID)
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]string{"summary": "", "error": err.Error()})
			return
		}

		writeJSON(w, http.StatusOK, map[string]string{"summary": summary, "error": ""})
	}
}

func handleGeneratePreset(generateFn func(ctx context.Context, description string) (config.Preset, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		var body struct {
			Description string `json:"description"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if body.Description == "" {
			writeJSONError(w, http.StatusBadRequest, "description is required")
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()

		preset, err := generateFn(ctx, body.Description)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}

		writeJSON(w, http.StatusOK, preset)
	}
}

func handleRefinePreset(cfgStore *config.Store, refineFn func(ctx context.Context, current config.Preset, feedback string) (config.Preset, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		var body struct {
			Name     string `json:"name"`
			Feedback string `json:"feedback"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if body.Name == "" {
			writeJSONError(w, http.StatusBadRequest, "name is required")
			return
		}
		if body.Feedback == "" {
			writeJSONError(w, http.StatusBadRequest, "feedback is required")
			return
		}

		cfg := cfgStore.Get()
		current, ok := cfg.Summarization.Presets[body.Name]
		if !ok {
			writeJSONError(w, http.StatusNotFound, fmt.Sprintf("preset %q not found", body.Name))
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()

		refined, err := refineFn(ctx, current, body.Feedback)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}

		writeJSON(w, http.StatusOK, refined)
	}
}

func registerRestoreRoutes(mux *http.ServeMux, controls *ControlHooks) {
	mux.HandleFunc("POST /api/restore/gdrive", func(w http.ResponseWriter, r *http.Request) {
		if controls.RestoreFromGDrive == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "Google Drive sync is not configured")
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
		defer cancel()

		result, err := controls.RestoreFromGDrive(ctx)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("restore: %v", err))
			return
		}

		writeJSON(w, http.StatusOK, result)
	})
}
