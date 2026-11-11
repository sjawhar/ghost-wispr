package server

import "encoding/json"

// configSchema documents the GC settings of GET and PATCH /api/config (a
// JSON merge patch has the same shape); the other settings are untyped.
var configSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"gc": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"enabled": map[string]any{
					"type":        "boolean",
					"description": "Run garbage collection every hour",
				},
				"max_age_days": map[string]any{
					"type":        "integer",
					"minimum":     0,
					"description": "Age policy: delete ended sessions with a completed summary (synced ones only while Drive sync is on) older than this many days, counted from when they started or, for a session restored from Drive, from its restore. 0 means no limit: no session is deleted for its age.",
				},
				"max_audio_size_mb": map[string]any{
					"type":        "integer",
					"minimum":     0,
					"description": "Audio budget: past it, GC removes the oldest sessions' audio files and keeps the sessions. 0 means no limit: no audio is removed to free space.",
				},
			},
		},
	},
}

// OpenAPISpec returns the OpenAPI 3.1.0 specification for the Ghost Wispr API.
func OpenAPISpec() map[string]any {
	return map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title":       "Ghost Wispr API",
			"description": "REST API for Ghost Wispr meeting transcription and summarization",
			"version":     "1.0.0",
		},
		"servers": []map[string]any{
			{
				"url":         "http://localhost:8080",
				"description": "Development server",
			},
		},
		"paths": map[string]any{
			// Health check endpoints
			"/healthz/live": map[string]any{
				"get": map[string]any{
					"summary":     "Liveness probe",
					"description": "Check if the service is running",
					"tags":        []string{"health"},
					"responses": map[string]any{
						"200": map[string]any{
							"description": "Service is alive",
						},
					},
				},
			},
			"/healthz/ready": map[string]any{
				"get": map[string]any{
					"summary":     "Readiness probe",
					"description": "Check if the service is ready to handle requests",
					"tags":        []string{"health"},
					"responses": map[string]any{
						"200": map[string]any{
							"description": "Service is ready",
						},
					},
				},
			},

			// Version endpoint
			"/api/version": map[string]any{
				"get": map[string]any{
					"summary":     "Get version info",
					"description": "Returns build version, commit hash, and build time",
					"tags":        []string{"system"},
					"security":    []map[string][]string{{"basicAuth": {}}},
					"responses": map[string]any{
						"200": map[string]any{
							"description": "Version information",
							"content": map[string]any{
								"application/json": map[string]any{
									"schema": map[string]any{
										"type": "object",
										"properties": map[string]any{
											"version":    map[string]any{"type": "string"},
											"commit":     map[string]any{"type": "string"},
											"build_time": map[string]any{"type": "string"},
										},
									},
								},
							},
						},
					},
				},
			},

			// Search endpoint
			"/api/search": map[string]any{
				"get": map[string]any{
					"summary":     "Full-text search",
					"description": "Search across all sessions with optional date and preset filters",
					"tags":        []string{"search"},
					"security":    []map[string][]string{{"basicAuth": {}}},
					"parameters": []map[string]any{
						{
							"name":        "q",
							"in":          "query",
							"description": "Search query",
							"required":    true,
							"schema":      map[string]any{"type": "string"},
						},
						{
							"name":        "date_from",
							"in":          "query",
							"description": "Filter sessions from this date (RFC3339Nano format)",
							"required":    false,
							"schema":      map[string]any{"type": "string"},
						},
						{
							"name":        "date_to",
							"in":          "query",
							"description": "Filter sessions until this date (RFC3339Nano format)",
							"required":    false,
							"schema":      map[string]any{"type": "string"},
						},
						{
							"name":        "preset",
							"in":          "query",
							"description": "Filter by summary preset name",
							"required":    false,
							"schema":      map[string]any{"type": "string"},
						},
						{
							"name":        "speaker",
							"in":          "query",
							"description": "Filter by speaker name or index (e.g., 'Ben' or '0')",
							"required":    false,
							"schema":      map[string]any{"type": "string"},
						},
					},
					"responses": map[string]any{
						"200": map[string]any{
							"description": "Search results",
						},
					},
				},
			},

			// Semantic search endpoint
			"/api/search/semantic": map[string]any{
				"get": map[string]any{
					"summary":     "Semantic search",
					"description": "Search across sessions using semantic similarity (embedding-based)",
					"tags":        []string{"search"},
					"security":    []map[string][]string{{"basicAuth": {}}},
					"parameters": []map[string]any{
						{
							"name":        "q",
							"in":          "query",
							"description": "Query text for semantic search",
							"required":    true,
							"schema":      map[string]any{"type": "string"},
						},
						{
							"name":        "limit",
							"in":          "query",
							"description": "Maximum number of results (default 10)",
							"required":    false,
							"schema":      map[string]any{"type": "integer"},
						},
						{
							"name":        "date_from",
							"in":          "query",
							"description": "Filter results from this date (RFC3339Nano format)",
							"required":    false,
							"schema":      map[string]any{"type": "string"},
						},
						{
							"name":        "date_to",
							"in":          "query",
							"description": "Filter results until this date (RFC3339Nano format)",
							"required":    false,
							"schema":      map[string]any{"type": "string"},
						},
					},
					"responses": map[string]any{
						"200": map[string]any{
							"description": "Semantic search results with similarity scores",
						},
						"501": map[string]any{
							"description": "Semantic search unavailable (no embedding provider configured)",
						},
					},
				},
			},

			// Event polling endpoint
			"/api/events": map[string]any{
				"get": map[string]any{
					"summary":     "Poll for events",
					"description": "Get events with cursor-based pagination and optional type filtering",
					"tags":        []string{"events"},
					"security":    []map[string][]string{{"basicAuth": {}}},
					"parameters": []map[string]any{
						{
							"name":        "cursor",
							"in":          "query",
							"description": "Event ID cursor for pagination (default 0)",
							"required":    false,
							"schema":      map[string]any{"type": "integer"},
						},
						{
							"name":        "limit",
							"in":          "query",
							"description": "Maximum number of events to return (default 50, max 200)",
							"required":    false,
							"schema":      map[string]any{"type": "integer"},
						},
						{
							"name":        "types",
							"in":          "query",
							"description": "Comma-separated event types to filter (e.g., 'session_ended,summary_ready')",
							"required":    false,
							"schema":      map[string]any{"type": "string"},
						},
					},
					"responses": map[string]any{
						"200": map[string]any{
							"description": "Events with pagination info",
						},
					},
				},
			},
			// Session management endpoints
			"/api/sessions/start": map[string]any{
				"post": map[string]any{
					"summary":     "Start a new session",
					"description": "Begin recording a new meeting session",
					"tags":        []string{"sessions"},
					"security":    []map[string][]string{{"basicAuth": {}}},
					"requestBody": map[string]any{
						"content": map[string]any{
							"application/json": map[string]any{
								"schema": map[string]any{
									"type": "object",
									"properties": map[string]any{
										"title_hint": map[string]any{"type": "string"},
									},
								},
							},
						},
					},
					"responses": map[string]any{
						"200": map[string]any{
							"description": "Session started",
						},
						"409": map[string]any{
							"description": "A session is already active, or capture is paused",
						},
					},
				},
			},
			"/api/sessions/current/stop": map[string]any{
				"post": map[string]any{
					"summary":     "Stop current session",
					"description": "Stop the currently active session",
					"tags":        []string{"sessions"},
					"security":    []map[string][]string{{"basicAuth": {}}},
					"responses": map[string]any{
						"200": map[string]any{
							"description": "Session stopped",
						},
					},
				},
			},
			"/api/sessions/{id}/stop": map[string]any{
				"post": map[string]any{
					"summary":     "Stop specific session",
					"description": "Stop a specific session by ID",
					"tags":        []string{"sessions"},
					"security":    []map[string][]string{{"basicAuth": {}}},
					"parameters": []map[string]any{
						{
							"name":        "id",
							"in":          "path",
							"description": "Session ID",
							"required":    true,
							"schema":      map[string]any{"type": "string"},
						},
					},
					"responses": map[string]any{
						"200": map[string]any{
							"description": "Session stopped",
						},
					},
				},
			},

			// Session listing and retrieval
			"/api/sessions": map[string]any{
				"get": map[string]any{
					"summary":     "List sessions",
					"description": "Get sessions for a specific date",
					"tags":        []string{"sessions"},
					"security":    []map[string][]string{{"basicAuth": {}}},
					"parameters": []map[string]any{
						{
							"name":        "date",
							"in":          "query",
							"description": "Date in YYYY-MM-DD format (defaults to today)",
							"required":    false,
							"schema":      map[string]any{"type": "string"},
						},
						{
							"name":        "include_discarded",
							"in":          "query",
							"description": "Include discarded sessions",
							"required":    false,
							"schema":      map[string]any{"type": "boolean"},
						},
					},
					"responses": map[string]any{
						"200": map[string]any{
							"description": "List of sessions",
						},
					},
				},
			},
			"/api/sessions/aggregate": map[string]any{
				"get": map[string]any{
					"summary":     "Aggregate sessions",
					"description": "Get aggregated session statistics grouped by date or preset",
					"tags":        []string{"sessions"},
					"security":    []map[string][]string{{"basicAuth": {}}},
					"parameters": []map[string]any{
						{
							"name":        "group_by",
							"in":          "query",
							"description": "Group by 'date' or 'preset' (defaults to 'date')",
							"required":    false,
							"schema":      map[string]any{"type": "string", "enum": []string{"date", "preset"}},
						},
						{
							"name":        "date_from",
							"in":          "query",
							"description": "Filter sessions from this date",
							"required":    false,
							"schema":      map[string]any{"type": "string"},
						},
						{
							"name":        "date_to",
							"in":          "query",
							"description": "Filter sessions until this date",
							"required":    false,
							"schema":      map[string]any{"type": "string"},
						},
						{
							"name":        "preset",
							"in":          "query",
							"description": "Filter by summary preset name",
							"required":    false,
							"schema":      map[string]any{"type": "string"},
						},
					},
					"responses": map[string]any{
						"200": map[string]any{
							"description": "Aggregated session data",
						},
					},
				},
			},
			"/api/sessions/{id}": map[string]any{
				"get": map[string]any{
					"summary":     "Get session details",
					"description": "Retrieve full details and segments for a specific session",
					"tags":        []string{"sessions"},
					"security":    []map[string][]string{{"basicAuth": {}}},
					"parameters": []map[string]any{
						{
							"name":        "id",
							"in":          "path",
							"description": "Session ID",
							"required":    true,
							"schema":      map[string]any{"type": "string"},
						},
					},
					"responses": map[string]any{
						"200": map[string]any{
							"description": "Session details with segments",
						},
					},
				},
				"patch": map[string]any{
					"summary":     "Update session",
					"description": "Update session title",
					"tags":        []string{"sessions"},
					"security":    []map[string][]string{{"basicAuth": {}}},
					"parameters": []map[string]any{
						{
							"name":        "id",
							"in":          "path",
							"description": "Session ID",
							"required":    true,
							"schema":      map[string]any{"type": "string"},
						},
					},
					"requestBody": map[string]any{
						"content": map[string]any{
							"application/json": map[string]any{
								"schema": map[string]any{
									"type": "object",
									"properties": map[string]any{
										"title": map[string]any{"type": "string"},
									},
								},
							},
						},
					},
					"responses": map[string]any{
						"200": map[string]any{
							"description": "Updated session",
						},
					},
				},
				"delete": map[string]any{
					"summary":     "Delete session",
					"description": "Delete a session and its associated audio files",
					"tags":        []string{"sessions"},
					"security":    []map[string][]string{{"basicAuth": {}}},
					"parameters": []map[string]any{
						{
							"name":        "id",
							"in":          "path",
							"description": "Session ID",
							"required":    true,
							"schema":      map[string]any{"type": "string"},
						},
					},
					"responses": map[string]any{
						"204": map[string]any{
							"description": "Session deleted",
						},
					},
				},
			},

			// Session merge
			"/api/sessions/merge": map[string]any{
				"post": map[string]any{
					"summary":     "Merge sessions",
					"description": "Merge multiple sessions into one",
					"tags":        []string{"sessions"},
					"security":    []map[string][]string{{"basicAuth": {}}},
					"requestBody": map[string]any{
						"content": map[string]any{
							"application/json": map[string]any{
								"schema": map[string]any{
									"type": "object",
									"properties": map[string]any{
										"session_ids": map[string]any{
											"type":  "array",
											"items": map[string]any{"type": "string"},
										},
									},
									"required": []string{"session_ids"},
								},
							},
						},
					},
					"responses": map[string]any{
						"200": map[string]any{
							"description": "Merged session",
						},
					},
				},
			},

			// Session audio and context
			"/api/sessions/{id}/audio": map[string]any{
				"get": map[string]any{
					"summary":     "Get session audio",
					"description": "Download audio file for a session",
					"tags":        []string{"sessions"},
					"security":    []map[string][]string{{"basicAuth": {}}},
					"parameters": []map[string]any{
						{
							"name":        "id",
							"in":          "path",
							"description": "Session ID",
							"required":    true,
							"schema":      map[string]any{"type": "string"},
						},
					},
					"responses": map[string]any{
						"200": map[string]any{
							"description": "Audio file",
							"content": map[string]any{
								"audio/mpeg": map[string]any{},
								"audio/wav":  map[string]any{},
							},
						},
					},
				},
			},

			// Session segments endpoint
			"/api/sessions/{id}/segments": map[string]any{
				"get": map[string]any{
					"summary":     "Get session segments",
					"description": "Get transcript segments for a session with optional speaker filter",
					"tags":        []string{"sessions"},
					"security":    []map[string][]string{{"basicAuth": {}}},
					"parameters": []map[string]any{
						{
							"name":        "id",
							"in":          "path",
							"description": "Session ID",
							"required":    true,
							"schema":      map[string]any{"type": "string"},
						},
						{
							"name":        "speaker",
							"in":          "query",
							"description": "Filter by speaker name or index (e.g., 'Ben' or '0')",
							"required":    false,
							"schema":      map[string]any{"type": "string"},
						},
					},
					"responses": map[string]any{
						"200": map[string]any{
							"description": "Transcript segments",
						},
					},
				},
			},
			"/api/sessions/{id}/segments/{segmentID}": map[string]any{
				"delete": map[string]any{
					"summary":     "Delete one session segment",
					"description": "Permanently remove one ended session segment from the transcript and recording.",
					"tags":        []string{"sessions"},
					"security":    []map[string][]string{{"basicAuth": {}}},
					"parameters": []map[string]any{
						{
							"name":        "id",
							"in":          "path",
							"description": "Session ID",
							"required":    true,
							"schema":      map[string]any{"type": "string"},
						},
						{
							"name":        "segmentID",
							"in":          "path",
							"description": "Segment ID",
							"required":    true,
							"schema":      map[string]any{"type": "integer", "format": "int64"},
						},
					},
					"responses": map[string]any{
						"204": map[string]any{"description": "Segment deleted"},
						"404": map[string]any{"description": "Session or segment not found"},
						"409": map[string]any{"description": "Session is still recording"},
						"422": map[string]any{"description": "Segment could not be found or removed cleanly"},
					},
				},
			},
			"/api/sessions/{id}/context": map[string]any{
				"get": map[string]any{
					"summary":     "Get context window",
					"description": "Get transcript segments around a specific query match",
					"tags":        []string{"sessions"},
					"security":    []map[string][]string{{"basicAuth": {}}},
					"parameters": []map[string]any{
						{
							"name":        "id",
							"in":          "path",
							"description": "Session ID",
							"required":    true,
							"schema":      map[string]any{"type": "string"},
						},
						{
							"name":        "q",
							"in":          "query",
							"description": "Query text to find in transcript",
							"required":    true,
							"schema":      map[string]any{"type": "string"},
						},
						{
							"name":        "seconds",
							"in":          "query",
							"description": "Context window size in seconds (defaults to 300)",
							"required":    false,
							"schema":      map[string]any{"type": "number"},
						},
					},
					"responses": map[string]any{
						"200": map[string]any{
							"description": "Context segments",
						},
					},
				},
			},

			// Session actions
			"/api/sessions/{id}/resummarize": map[string]any{
				"post": map[string]any{
					"summary":     "Resummarize session",
					"description": "Generate a new summary for a session with optional preset. The session keeps its title and current summary until the new one is stored; if making it fails, it keeps them, and summary_status becomes failed with summary_error saying why (error_message keeps the last sync's error); a completed summary clears summary_error",
					"tags":        []string{"sessions"},
					"security":    []map[string][]string{{"basicAuth": {}}},
					"parameters": []map[string]any{
						{
							"name":        "id",
							"in":          "path",
							"description": "Session ID",
							"required":    true,
							"schema":      map[string]any{"type": "string"},
						},
					},
					"requestBody": map[string]any{
						"content": map[string]any{
							"application/json": map[string]any{
								"schema": map[string]any{
									"type": "object",
									"properties": map[string]any{
										"preset": map[string]any{"type": "string"},
									},
								},
							},
						},
					},
					"responses": map[string]any{
						"202": map[string]any{
							"description": "Resummarization triggered",
						},
					},
				},
			},
			"/api/sessions/{id}/retry-summary": map[string]any{
				"post": map[string]any{
					"summary":     "Retry summary generation",
					"description": "Retry generating summary for a session",
					"tags":        []string{"sessions"},
					"security":    []map[string][]string{{"basicAuth": {}}},
					"parameters": []map[string]any{
						{
							"name":        "id",
							"in":          "path",
							"description": "Session ID",
							"required":    true,
							"schema":      map[string]any{"type": "string"},
						},
					},
					"responses": map[string]any{
						"200": map[string]any{
							"description": "Retry triggered",
						},
					},
				},
			},
			"/api/sessions/{id}/retry-sync": map[string]any{
				"post": map[string]any{
					"summary":     "Retry sync",
					"description": "Start syncing the session to Google Drive again, in the background: a session waiting for its retry, or whose retries ran out (failed), or whose Drive folder was deleted (REMOTE_DELETED) once Drive shows the folder restored. A session synced before keeps its Drive folder.",
					"tags":        []string{"sessions"},
					"security":    []map[string][]string{{"basicAuth": {}}},
					"parameters": []map[string]any{
						{
							"name":        "id",
							"in":          "path",
							"description": "Session ID",
							"required":    true,
							"schema":      map[string]any{"type": "string"},
						},
					},
					"responses": map[string]any{
						"200": map[string]any{
							"description": "Sync started",
						},
						"404": map[string]any{
							"description": "No such session",
						},
						"409": map[string]any{
							"description": "No sync can start for this session (already synced, being synced now, or its Drive folder is still in the trash or gone); the error says why",
						},
						"503": map[string]any{
							"description": "Google Drive sync is off, or Drive did not answer whether a deleted session's folder is back; the error says which",
						},
					},
				},
			},
			"/api/sessions/{id}/retry-refinement": map[string]any{
				"post": map[string]any{
					"summary":     "Retry refinement",
					"description": "Retry refining session summary",
					"tags":        []string{"sessions"},
					"security":    []map[string][]string{{"basicAuth": {}}},
					"parameters": []map[string]any{
						{
							"name":        "id",
							"in":          "path",
							"description": "Session ID",
							"required":    true,
							"schema":      map[string]any{"type": "string"},
						},
					},
					"responses": map[string]any{
						"200": map[string]any{
							"description": "Retry triggered",
						},
						"409": map[string]any{
							"description": "The session has no saved recording to refine (refinement_status no_audio)",
						},
					},
				},
			},

			// Dates endpoint
			"/api/dates": map[string]any{
				"get": map[string]any{
					"summary":     "Get available dates",
					"description": "List all dates with recorded sessions",
					"tags":        []string{"sessions"},
					"security":    []map[string][]string{{"basicAuth": {}}},
					"responses": map[string]any{
						"200": map[string]any{
							"description": "List of dates",
						},
					},
				},
			},

			// Control endpoints
			"/api/pause": map[string]any{
				"post": map[string]any{
					"summary":     "Pause recording",
					"description": "Stop microphone capture: no audio reaches the recording or Deepgram, the microphone stops, the active session ends, and no session starts until resume",
					"tags":        []string{"control"},
					"security":    []map[string][]string{{"basicAuth": {}}},
					"responses": map[string]any{
						"204": map[string]any{
							"description": "Paused",
						},
						"500": map[string]any{
							"description": "The pause was applied but its persistent state could not be saved",
						},
					},
				},
			},
			"/api/resume": map[string]any{
				"post": map[string]any{
					"summary":     "Resume recording",
					"description": "Resume a persisted pause. A configured, present recording light confirms red before the request returns, then confirms it again before the gate opens. A later failure keeps capture paused and sends status_changed to clients. Audio flows once the paused session has ended and the microphone restarts.",
					"tags":        []string{"control"},
					"security":    []map[string][]string{{"basicAuth": {}}},
					"responses": map[string]any{
						"204": map[string]any{
							"description": "Resume request accepted; clients receive status_changed if a later gate-open check refuses capture",
						},
						"500": map[string]any{
							"description": "The recording light could not be confirmed at request time or persistent state could not be saved; capture stays paused",
						},
					},
				},
			},
			"/api/session/end": map[string]any{
				"post": map[string]any{
					"summary":     "End current session",
					"description": "End the currently active session",
					"tags":        []string{"control"},
					"security":    []map[string][]string{{"basicAuth": {}}},
					"responses": map[string]any{
						"204": map[string]any{
							"description": "Session ended",
						},
					},
				},
			},

			// Status endpoint
			"/api/status": map[string]any{
				"get": map[string]any{
					"summary":     "Get system status",
					"description": "Get current system status: paused (the pause switch), capturing (the microphone is capturing and audio is flowing), capture_state (paused, resuming, stalled, mic_unavailable or capturing), kept_raw_recordings and kept_raw_bytes (raw recordings in the audio directory that could not be saved and wait for recovery or deletion by hand; a warning once they take more than a tenth of the audio budget, or as soon as there is one when the budget is 0, no limit), gc_hold (null, or the last GC pass held back because it could not check that Drive still has the sessions: reason, since, disk_used_bytes and disk_total_bytes of the audio directory's filesystem; with a warning), unverified_sessions (sessions synced by an earlier version that GC keeps until a sync re-checks their documents on Drive; with a warning while there are any), and the active session",
					"tags":        []string{"system"},
					"security":    []map[string][]string{{"basicAuth": {}}},
					"responses": map[string]any{
						"200": map[string]any{
							"description": "System status",
						},
					},
				},
			},

			// Presets endpoint
			"/api/presets": map[string]any{
				"get": map[string]any{
					"summary":     "Get available presets",
					"description": "List all available summary presets",
					"tags":        []string{"config"},
					"security":    []map[string][]string{{"basicAuth": {}}},
					"responses": map[string]any{
						"200": map[string]any{
							"description": "Available presets",
						},
					},
				},
			},

			// Config endpoints
			"/api/config": map[string]any{
				"get": map[string]any{
					"summary":     "Get configuration",
					"description": "Get current application configuration",
					"tags":        []string{"config"},
					"security":    []map[string][]string{{"basicAuth": {}}},
					"responses": map[string]any{
						"200": map[string]any{
							"description": "Configuration",
							"content": map[string]any{
								"application/json": map[string]any{"schema": configSchema},
							},
						},
					},
				},
				"patch": map[string]any{
					"summary":     "Update configuration",
					"description": "Update application configuration (JSON merge patch)",
					"tags":        []string{"config"},
					"security":    []map[string][]string{{"basicAuth": {}}},
					"requestBody": map[string]any{
						"content": map[string]any{
							"application/json": map[string]any{"schema": configSchema},
						},
					},
					"responses": map[string]any{
						"200": map[string]any{
							"description": "Updated configuration",
							"content": map[string]any{
								"application/json": map[string]any{"schema": configSchema},
							},
						},
						"400": map[string]any{
							"description": "An invalid setting, such as a negative GC limit; nothing is changed",
						},
					},
				},
			},
			"/api/config/presets/{name}/test": map[string]any{
				"post": map[string]any{
					"summary":     "Test preset",
					"description": "Test a summary preset on a specific session",
					"tags":        []string{"config"},
					"security":    []map[string][]string{{"basicAuth": {}}},
					"parameters": []map[string]any{
						{
							"name":        "name",
							"in":          "path",
							"description": "Preset name",
							"required":    true,
							"schema":      map[string]any{"type": "string"},
						},
					},
					"requestBody": map[string]any{
						"content": map[string]any{
							"application/json": map[string]any{
								"schema": map[string]any{
									"type": "object",
									"properties": map[string]any{
										"session_id": map[string]any{"type": "string"},
									},
									"required": []string{"session_id"},
								},
							},
						},
					},
					"responses": map[string]any{
						"200": map[string]any{
							"description": "Test result",
						},
					},
				},
			},
			"/api/config/presets/generate": map[string]any{
				"post": map[string]any{
					"summary":     "Generate preset",
					"description": "Generate a new summary preset from description",
					"tags":        []string{"config"},
					"security":    []map[string][]string{{"basicAuth": {}}},
					"requestBody": map[string]any{
						"content": map[string]any{
							"application/json": map[string]any{
								"schema": map[string]any{
									"type": "object",
									"properties": map[string]any{
										"description": map[string]any{"type": "string"},
									},
									"required": []string{"description"},
								},
							},
						},
					},
					"responses": map[string]any{
						"200": map[string]any{
							"description": "Generated preset",
						},
					},
				},
			},
			"/api/config/presets/refine": map[string]any{
				"post": map[string]any{
					"summary":     "Refine preset",
					"description": "Refine an existing preset based on feedback",
					"tags":        []string{"config"},
					"security":    []map[string][]string{{"basicAuth": {}}},
					"requestBody": map[string]any{
						"content": map[string]any{
							"application/json": map[string]any{
								"schema": map[string]any{
									"type": "object",
									"properties": map[string]any{
										"name":     map[string]any{"type": "string"},
										"feedback": map[string]any{"type": "string"},
									},
									"required": []string{"name", "feedback"},
								},
							},
						},
					},
					"responses": map[string]any{
						"200": map[string]any{
							"description": "Refined preset",
						},
					},
				},
			},

			// Restore endpoint
			"/api/restore/gdrive": map[string]any{
				"post": map[string]any{
					"summary":     "Restore from Google Drive",
					"description": "Restore sessions from Google Drive backup",
					"tags":        []string{"backup"},
					"security":    []map[string][]string{{"basicAuth": {}}},
					"responses": map[string]any{
						"200": map[string]any{
							"description": "Restore result",
						},
					},
				},
			},

			// Logs endpoint
			"/api/logs": map[string]any{
				"get": map[string]any{
					"summary":     "Get logs",
					"description": "Retrieve application logs",
					"tags":        []string{"system"},
					"security":    []map[string][]string{{"basicAuth": {}}},
					"parameters": []map[string]any{
						{
							"name":        "level",
							"in":          "query",
							"description": "Log level filter (debug, info, warn, error)",
							"required":    false,
							"schema":      map[string]any{"type": "string"},
						},
						{
							"name":        "limit",
							"in":          "query",
							"description": "Maximum number of log entries to return",
							"required":    false,
							"schema":      map[string]any{"type": "integer"},
						},
						{
							"name":        "since",
							"in":          "query",
							"description": "Return logs since this timestamp (RFC3339Nano format)",
							"required":    false,
							"schema":      map[string]any{"type": "string"},
						},
					},
					"responses": map[string]any{
						"200": map[string]any{
							"description": "Log entries",
						},
					},
				},
			},

			// Diagnostic endpoints
			"/api/diagnostic/mic": map[string]any{
				"post": map[string]any{
					"summary":     "Diagnose microphone",
					"description": "Run microphone diagnostics",
					"tags":        []string{"diagnostic"},
					"security":    []map[string][]string{{"basicAuth": {}}},
					"responses": map[string]any{
						"200": map[string]any{
							"description": "Diagnostic report",
						},
					},
				},
			},

			// Test endpoints (only in test mode)
			"/api/test/fault/deepgram-disconnect": map[string]any{
				"post": map[string]any{
					"summary":     "Inject Deepgram disconnect fault",
					"description": "Trigger a Deepgram disconnection (test mode only)",
					"tags":        []string{"test"},
					"security":    []map[string][]string{{"basicAuth": {}}},
					"responses": map[string]any{
						"200": map[string]any{
							"description": "Fault injected",
						},
					},
				},
			},

			// OpenAPI spec endpoint
			"/api/openapi.json": map[string]any{
				"get": map[string]any{
					"summary":     "Get OpenAPI specification",
					"description": "Returns the OpenAPI 3.1.0 specification for this API",
					"tags":        []string{"system"},
					"responses": map[string]any{
						"200": map[string]any{
							"description": "OpenAPI specification",
							"content": map[string]any{
								"application/json": map[string]any{},
							},
						},
					},
				},
			},
		},
		"components": map[string]any{
			"securitySchemes": map[string]any{
				"basicAuth": map[string]any{
					"type":        "http",
					"scheme":      "basic",
					"description": "HTTP Basic Authentication with username 'ghost-wispr' and token from GHOST_WISPR_AUTH_TOKEN environment variable",
				},
			},
		},
		"security": []map[string][]string{
			{"basicAuth": {}},
		},
	}
}

// OpenAPISpecJSON returns the OpenAPI spec as JSON bytes.
func OpenAPISpecJSON() ([]byte, error) {
	return json.Marshal(OpenAPISpec())
}
