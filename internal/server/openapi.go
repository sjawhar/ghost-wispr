package server

import (
	"encoding/json"

	"github.com/sjawhar/ghost-wispr/internal/config"
)

// providerSchema is one model provider of GET /api/config's providers: an
// endpoint, the wire API it speaks and how the unit signs in to it. It names
// no secret.
var providerSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"api": map[string]any{
			"type":        "string",
			"enum":        config.APIs,
			"description": "The wire API the provider speaks: openai (the OpenAI API), anthropic (the Anthropic API), vertex (Vertex AI), gemini (the Gemini API), deepgram, elevenlabs or google-tts (Google Cloud Text-to-Speech)",
		},
		"url":      map[string]any{"type": "string", "description": "The API's base URL, such as a model gateway's; absent is the API's public address"},
		"project":  map[string]any{"type": "string", "description": "Vertex AI: the Google Cloud project"},
		"location": map[string]any{"type": "string", "description": "Vertex AI: the location, such as global or us"},
		"auth": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"kind": map[string]any{
					"type":        "string",
					"enum":        config.AuthKinds,
					"description": "How the unit signs in: api_key (a key from the variable env names), google (a Google access token, by workload identity or Application Default Credentials), google_id_token (a Google ID token for audience) or openai_wif (an OpenAI access token in exchange for the unit's Google ID token)",
				},
				"env":                  map[string]any{"type": "string", "description": "api_key: the environment variable that holds the key; the key itself is never served"},
				"audience":             map[string]any{"type": "string", "description": "google_id_token: the ID token's audience"},
				"identity_provider_id": map[string]any{"type": "string", "description": "openai_wif: OpenAI's identity provider id, not a secret"},
				"service_account_id":   map[string]any{"type": "string", "description": "openai_wif: OpenAI's service account id, not a secret"},
			},
		},
	},
}

// configSchema documents the settings of GET and PATCH /api/config (a JSON
// merge patch has the same shape) that the settings page reads and changes,
// and the read-only fields GET reports; the other settings are untyped.
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
		"models": map[string]any{
			"type":                 "object",
			"readOnly":             true,
			"description":          "GET only: what each role calls as the running process resolved its configuration, environment overrides included: the provider/model of live_transcription, batch_transcription, summarization, embedding, concierge and tts; tts_voice, the tts role's voice; gptlive, voice B's provider/model; and dgagent_provider, voice C's provider. An empty value is a role the configuration does not set. A PATCH cannot set it.",
			"additionalProperties": map[string]any{"type": "string"},
		},
		"providers": map[string]any{
			"type":                 "object",
			"readOnly":             true,
			"description":          "GET only: the model providers the configuration names (providers:), by name. Each role's provider/model names one. No secret: an api_key provider names the variable that holds its key. A PATCH cannot set it; GET /api/providers says whether each one signs in.",
			"additionalProperties": providerSchema,
		},
		"provider_keys": map[string]any{
			"type":                 "object",
			"writeOnly":            true,
			"description":          "PATCH only: the key of each named provider that signs in with one (auth.kind api_key), saved under the variable its auth.env names, in the unit's .env file. A provider that signs in another way takes no key: the PATCH is refused with 400. GET never serves a key.",
			"additionalProperties": map[string]any{"type": "string"},
		},
		"summarization": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"model":   map[string]any{"type": "string", "description": "Summaries and titles: the provider/model, such as vertex/gemini-3.5-flash-lite"},
				"presets": map[string]any{"type": "object", "description": "The summary presets, by name. In a PATCH, null deletes one; a preset's fields left out keep their values, and a model or description of \"\" clears it (a preset with no model uses summarization.model)"},
			},
		},
		"tts": map[string]any{
			"type":        "object",
			"description": "Text-to-speech: what voice A and POST /api/tts/speak speak with. A change applies from the next speech, with no restart.",
			"properties": map[string]any{
				"model": map[string]any{"type": "string", "description": "The provider/model, such as vertex-us/gemini-3.8-flash-tts; empty turns text-to-speech off. A model no provider serves is refused with 400. A new model speaks in its own default voice (voice becomes empty) unless the same patch names a voice."},
				"voice": map[string]any{"type": "string", "description": "The model's voice, such as Kore; empty is the model's default"},
			},
		},
		"recording_rules": map[string]any{
			"type":        "array",
			"readOnly":    true,
			"items":       map[string]any{"type": "string"},
			"description": "GET only: the rules the web UI shows under Recording rules, each as written in recording_rules. GHOST_WISPR_RECORDING_RULES overrides it with a JSON list of strings, and a settings save (any PATCH /api/config) then writes the override's rules into the config file, where they stay after the variable is removed. Empty when none are set; the page then shows no Recording rules button. A PATCH cannot set it.",
		},
		"voice": map[string]any{
			"type":        "object",
			"description": "The meeting voice. A change applies from the next exchange, with no restart.",
			"properties": map[string]any{
				"enabled": map[string]any{
					"type":        "boolean",
					"description": "Turn the meeting voice on or off",
				},
				"engine": map[string]any{
					"type":        "string",
					"enum":        []string{"tts", "gptlive", "dgagent"},
					"description": "The voice that answers: tts (voice A, text-to-speech, with the tts role), gptlive (voice B, speech-to-speech: OpenAI GPT-Live) or dgagent (voice C, voice agent: Deepgram)",
				},
			},
		},
	},
}

// envoyPayloadSchema is the payload of every event sent to Envoy, the JSON
// object of strings Envoy's Ghost Wispr normalizer writes: no text of the
// session, only which session it is, when it ran, and where to read it.
var envoyPayloadSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"required":             []string{"event_type", "session_id", "started_at", "duration", "created_at"},
	"properties": map[string]any{
		"event_type":       map[string]any{"type": "string", "enum": []string{"session_started", "session_ended", "summary_ready"}},
		"session_id":       map[string]any{"type": "string"},
		"started_at":       map[string]any{"type": "string", "format": "date-time"},
		"ended_at":         map[string]any{"type": "string", "format": "date-time", "description": "Absent until the session ends"},
		"duration":         map[string]any{"type": "string", "description": "Whole seconds from start to end; 0 until the session ends"},
		"created_at":       map[string]any{"type": "string", "format": "date-time", "description": "When the event was sent"},
		"drive_folder_url": map[string]any{"type": "string", "format": "uri", "description": "The session's Google Drive folder; absent until the session has one"},
		"session_url":      map[string]any{"type": "string", "format": "uri", "description": "GET it for the session's current title, summary and transcript (public_url + /api/sessions/{id}); absent when public_url is not set"},
		"summary_preset":   map[string]any{"type": "string", "description": "summary_ready only"},
		"summary_model":    map[string]any{"type": "string", "description": "summary_ready only"},
		"version":          map[string]any{"type": "string", "description": "summary_ready only: the summary version this event announces"},
		"payload_type":     map[string]any{"type": "string", "description": "summary_ready only: summary_ready"},
	},
}

// OpenAPISpec returns the OpenAPI 3.1.0 specification for the Ghost Wispr API.
func OpenAPISpec() map[string]any {
	return map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title":       "Ghost Wispr API",
			"description": "REST API for Ghost Wispr meeting transcription and summarization. Every POST, PATCH, PUT and DELETE, and the /ws WebSocket handshake, whose Origin header names another site (or null) gets 403: only the host the request came to and the unit's public_url are accepted. A request with no Origin header, such as curl's, is accepted.",
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
					"description": "Check if the service is ready to handle requests. While capture is paused or resuming, a microphone that is closed and a Deepgram connection that is not made yet are reported as paused or resuming, not as faults: a unit that starts paused connects to Deepgram only when it resumes. Resuming lasts from a resume until the microphone has started, or the start failed, for at most 30 seconds, or for as long as the microphone waits for speech on the Jabra to end before it opens; Deepgram then connects in the background, and is reported as reconnecting until it has. With no Deepgram API key, Deepgram is a fault whether paused or not. While no audio reaches the recording (capture_state no_audio on /api/status), the microphone is reported as no_audio and the service is not ready.",
					"tags":        []string{"health"},
					"responses": map[string]any{
						"200": map[string]any{
							"description": "Service is ready",
						},
						"503": map[string]any{
							"description": "A component is not ready; the body names each component's state",
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
					"description": "Search across sessions using semantic similarity (embedding-based). The response model is the model reported by the embedding provider for that query.",
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
					"description": "Delete a session, the sessions merged into it, every stored event that names one of them (so /api/events no longer serves their transcript, summary, or refinement, summary and sync statuses), every GET /api/status warning that names one of them, their words in the search index, and their audio files. Every connected web client is sent a session_deleted websocket event with their ids only. The deleted rows and events are then erased from the database's write-ahead log and the pre-migration backup.",
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
						"409": map[string]any{
							"description": "The session is active, is part of another merge, or it or a merged source still has a deleted segment on Google Drive; delete it after its Drive files are replaced",
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
						"409": map[string]any{
							"description": "A session to merge is still recording, was already merged or discarded, or still has a deleted segment on Google Drive, the merged session's id is already taken, or a deleted session with that id still has audio being erased (try again in a few minutes); the message names the session and the reason",
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
					"description": "Permanently remove one ended session segment, by the action mode names. mode=text (\"Delete text\") removes the segment's words from the transcript, the refined transcript, the summary (made again), search, embeddings and stored events, on the session and every session merged into it, and records where the words are in the recording, which it leaves unchanged, so a later refinement leaves them out; the Drive transcript and summary are updated in place by the next sync, so their links stay and their version history keeps the old text. mode=text_and_audio (\"Delete text and audio\") also cuts the segment out of the recording and replaces the session's transcript, summary and recording files in Drive, deleting the old files with their version history, so their links change. The cut reaches 0.45 s past the segment's words on each side, into the neighbouring speech, and a batch transcription of the cut recording must hear none of the segment's words within 0.5 s of the cut beyond the neighbouring words there; a check must also hear the cut, or it is tried once more: it must hold words, and a word within 0.5 s of the cut when the cut leaves neighbouring words there. A cut with a word heard, or that no check could hear, is widened to 0.6 s, then 0.8 s, and the deletion refused, changing nothing, when the widest cut is too. The check, and a later \"Delete text\", use the batch model that timed the segment's words, which a merged session's recordings can each have their own of. Either refusal puts a status warning naming the session and the segment's time, never its words, kept in the database across restarts and merges until the segment's words are gone (by \"Delete text\", a later cut, or the session's deletion); a later cut of a touching neighbour trims the refused span rather than dropping it. Either mode refuses, changing nothing, a segment whose words cannot be found in the recording.",
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
						{
							"name":        "mode",
							"in":          "query",
							"description": "The deletion action: text (\"Delete text\") or text_and_audio (\"Delete text and audio\"). There is no default.",
							"required":    true,
							"schema":      map[string]any{"type": "string", "enum": []string{"text", "text_and_audio"}},
						},
						{
							"name":        "revision",
							"in":          "query",
							"description": "The transcript revision carried by the selected segment. Timed segments use a positive revision; an untimed segment from a source with no recording uses 0 with mode=text only. A changed revision returns 409; refresh and try again.",
							"required":    true,
							"schema":      map[string]any{"type": "integer", "minimum": 0},
						},
					},
					"responses": map[string]any{
						"204": map[string]any{"description": "Segment deleted"},
						"400": map[string]any{"description": "Missing or invalid mode or transcript revision"},
						"404": map[string]any{"description": "Session or segment not found"},
						"409": map[string]any{"description": "Session is still recording or refining, an earlier deletion is recovering, the transcript changed (refresh and try again), or part of this conversation's merge history can't be traced, so nothing in it can be deleted safely"},
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
			"/api/sessions/{id}/voice-tasks": map[string]any{
				"get": map[string]any{
					"summary":     "List the questions to agents",
					"description": "The questions the meeting voice sent the agents that hold issues, in this session and in the sessions merged into it, oldest first: the issue, the agent it went to, where each question stands, when it was asked and when its answer was spoken, and the agent's answer as written. The written answer is the agent's own text and can quote the asker. The unit keeps it with the session, and only deleting the session erases it.",
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
							"description": "The session's questions to agents, oldest first; an empty list when the voice asked none",
							"content": map[string]any{
								"application/json": map[string]any{
									"schema": map[string]any{
										"type": "array",
										"items": map[string]any{
											"type":     "object",
											"required": []string{"id", "issue_key", "holder", "state", "asked_at", "answer_written"},
											"properties": map[string]any{
												"id":             map[string]any{"type": "string", "description": "The Envoy event id of the message that carried the question"},
												"issue_key":      map[string]any{"type": "string", "description": "The issue the question is about"},
												"holder":         map[string]any{"type": "string", "description": "The agent the question went to, as its Envoy address (session:<id>)"},
												"state":          map[string]any{"type": "string", "enum": []string{"working", "done", "cancelled", "replaced"}, "description": "working: no answer yet; done: the agent answered; cancelled: the exchange that asked ended before an answer; replaced: a later question to the same agent took its place"},
												"asked_at":       map[string]any{"type": "string", "format": "date-time"},
												"spoken_at":      map[string]any{"type": "string", "format": "date-time", "description": "When the voice spoke the answer; absent until it does"},
												"answer_written": map[string]any{"type": "string", "description": "The agent's answer as written, in plain text; empty until it answers. A cancelled or replaced question keeps an answer that came after"},
											},
										},
									},
								},
							},
						},
						"403": map[string]any{"description": "Invalid session id"},
						"404": map[string]any{"description": "No such session"},
					},
				},
			},
			"/api/sessions/{id}/joined-agents": map[string]any{
				"get": map[string]any{
					"summary":     "List the agents that joined the meeting",
					"description": "The agents that joined this recording session's meeting and have not left, in the order they joined. An agent joins, asks for the floor and leaves with Envoy messages to the unit's own Envoy session; it leaves when it says so, when the session ends, or when Envoy lists it as not live for 10 minutes. The list lives only while the session records: an ended session lists none.",
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
							"description": "The joined agents; an empty list when none joined, or the meeting voice is off",
							"content": map[string]any{
								"application/json": map[string]any{
									"schema": map[string]any{
										"type": "array",
										"items": map[string]any{
											"type":     "object",
											"required": []string{"agent", "issue_key", "title", "joined_at"},
											"properties": map[string]any{
												"agent":     map[string]any{"type": "string", "description": "The agent's Envoy session id"},
												"issue_key": map[string]any{"type": "string", "description": "The issue it joined on; empty when it named none"},
												"title":     map[string]any{"type": "string", "description": "The issue's title, else the agent's Envoy title"},
												"joined_at": map[string]any{"type": "string", "format": "date-time"},
											},
										},
									},
								},
							},
						},
						"403": map[string]any{"description": "Invalid session id"},
					},
				},
			},
			"/api/voice/join-message": map[string]any{
				"get": map[string]any{
					"summary":     "The message that tells an agent to join the meeting",
					"description": "The message a person sends an agent through Dispatch (the Agents page, or dispatch send-to-agent) to have it join the meeting the unit records: the unit's Envoy session id and the whole floor protocol, so an agent with an older copy of the Ghost Wispr skill can follow it. It changes nothing.",
					"tags":        []string{"voice"},
					"security":    []map[string][]string{{"basicAuth": {}}},
					"parameters": []map[string]any{
						{
							"name":        "issue",
							"in":          "query",
							"description": "The issue the agent joins on, such as ABC-12; its key becomes a keyterm of the live transcript and names the agent in the room",
							"schema":      map[string]any{"type": "string"},
						},
					},
					"responses": map[string]any{
						"200": map[string]any{
							"description": "The join message",
							"content": map[string]any{
								"application/json": map[string]any{
									"schema": map[string]any{
										"type":       "object",
										"required":   []string{"message"},
										"properties": map[string]any{"message": map[string]any{"type": "string"}},
									},
								},
							},
						},
						"400": map[string]any{"description": "issue is not an issue key"},
						"503": map[string]any{"description": "No agent can join now: the meeting voice is off, or the unit has no Envoy session"},
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
						"202": map[string]any{
							"description": "Retry started in the background",
						},
						"409": map[string]any{
							"description": "No local recording or Drive copy can be restored for this session",
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
					"description": "Stop microphone capture: no audio reaches the recording or Deepgram, the active session ends, and no session starts until resume. A recording unit goes to listening while the paused listener can run (voice.paused_listener on, in a build that has it, with the Jabra's mute button attached): the light goes off and only the listener holds the microphone, for the start phrase alone. Otherwise, and always without the listener, it goes to closed: the microphone closes. A paused unit stays as it is.",
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
			"/api/close": map[string]any{
				"post": map[string]any{
					"summary":     "Close the microphone",
					"description": "Move the unit to closed from every state, as the Jabra's mute button does: no audio reaches the recording or Deepgram, the active session ends, the paused listener stops, the microphone closes, and the Jabra's ring shows Muted. Only a resume opens it again.",
					"tags":        []string{"control"},
					"security":    []map[string][]string{{"basicAuth": {}}},
					"responses": map[string]any{
						"204": map[string]any{
							"description": "Closed",
						},
						"500": map[string]any{
							"description": "The microphone closed but its persistent state could not be saved",
						},
						"503": map[string]any{
							"description": "This unit has no capture controls",
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
					"description": "Get current system status: paused (the pause switch, on in both listening and closed), capturing (the microphone is capturing and audio is flowing), capture_state (paused while the microphone is closed, listening while the switch is off and only the paused listener holds the microphone, for the start phrase alone, with the light off, light_unavailable while the switch is on but the recording light is missing or does not read back red, so nothing is captured until it does, resuming from a resume until the microphone has started, or the start failed, for at most 30 seconds, or for as long as the microphone waits for speech on the Jabra to end before it opens, stalled, mic_unavailable, no_audio once no audio has reached the recording for 2 seconds: the microphone is closed, and the recording light off, until it opens again, or capturing), kept_raw_recordings and kept_raw_bytes (every raw recording the app keeps in the audio directory: those of sessions that ended without a recording, which it tries again to save after every Drive sync sweep, those beside a saved recording, which it erases once Drive has it, and those it neither saves nor erases; a warning that tells these apart once they take more than a tenth of the audio budget, or as soon as there is one when the budget is 0, no limit), kept_raw_by_hand (the sessions of the raw recordings the app neither saves nor erases, which wait for a person to recover or delete them), gc_hold (null, or the last GC pass held back because it could not check that Drive still has the sessions: reason, since, disk_used_bytes and disk_total_bytes of the audio directory's filesystem; with a warning), unverified_sessions (sessions synced by an earlier version that GC keeps until a sync re-checks their documents on Drive; with a warning while there are any), unit_only_sessions (sessions marked REMOTE_DELETED whose audio is still on this unit: their Drive folder is gone or unreadable, so the unit holds the only copy; with a warning while there are any, until a retry syncs them), notices (what the web UI shows apart from warnings, about a state the configuration chose: while it names an Envoy hub (envoy_hub_url) with envoy_enabled false, one notice says no session event goes to the hub and each completed summary waits until Envoy is on again, and, with no concierge.envoy_url, that the voice cannot ask an issue's agent; empty otherwise), and the active session",
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
							"description": "An invalid setting, such as a negative GC limit, a key for a provider that does not sign in with one, a model no provider serves, or a field the settings do not have; nothing is changed",
						},
						"404": map[string]any{
							"description": "A change to a summarization preset the configuration does not hold, with no user_template for it: an update to a preset deleted elsewhere, or a new preset without the template that carries the transcript; nothing is changed",
						},
					},
				},
			},
			"/api/providers": map[string]any{
				"get": map[string]any{
					"summary":     "Check the model providers",
					"description": "Each provider the configuration names: how the unit signs in to it and whether that worked, and the roles in use that call it. A check signs in (a key is set, a Google token is minted, an OpenAI token is exchanged) and, where the API has a cheap read the credential must pass, makes it. Checks are kept for 5 minutes; refresh=1 checks every provider now. No key or token is served.",
					"tags":        []string{"config"},
					"security":    []map[string][]string{{"basicAuth": {}}},
					"parameters": []map[string]any{
						{
							"name":        "refresh",
							"in":          "query",
							"required":    false,
							"schema":      map[string]any{"type": "string", "enum": []string{"1"}},
							"description": "1 checks every provider now instead of keeping a check younger than 5 minutes",
						},
					},
					"responses": map[string]any{
						"200": map[string]any{
							"description": "The providers, by name",
							"content": map[string]any{
								"application/json": map[string]any{"schema": map[string]any{
									"type": "object",
									"properties": map[string]any{
										"providers": map[string]any{
											"type": "array",
											"items": map[string]any{
												"type": "object",
												"properties": map[string]any{
													"name":       map[string]any{"type": "string"},
													"api":        map[string]any{"type": "string", "enum": config.APIs},
													"url":        map[string]any{"type": "string", "description": "The base URL the unit calls"},
													"auth":       map[string]any{"type": "string", "enum": config.AuthKinds, "description": "The sign-in kind"},
													"sign_in":    map[string]any{"type": "string", "description": "How the provider signs in, in words"},
													"key_env":    map[string]any{"type": "string", "description": "api_key: the variable that holds the key"},
													"key_set":    map[string]any{"type": "boolean", "description": "api_key: whether the variable holds a key"},
													"checked":    map[string]any{"type": "boolean", "description": "A check ran"},
													"asked":      map[string]any{"type": "boolean", "description": "The provider's answers proved every model its roles call, or refused one; false when a minted or exchanged token is the check's only proof, when a Vertex AI provider cannot count one of its models (an embedding model's), when the check reads only the provider's account (Deepgram's projects, the ElevenLabs user, Cloud Text-to-Speech's voices) while a role calls a model on it, or when a role's model has no cheap call (a speech model), even after the provider answered other reads"},
													"ok":         map[string]any{"type": "boolean", "description": "The check signed in and, when asked, the provider took the credential; a provider whose read is its model list also lists, and answers a cheap call of, each model its roles call"},
													"message":    map[string]any{"type": "string", "description": "What the check found, or why it failed"},
													"checked_at": map[string]any{"type": "string", "format": "date-time"},
													"roles": map[string]any{
														"type":        "array",
														"items":       map[string]any{"type": "string"},
														"description": "The roles in use that call the provider, by their config keys, such as transcription.model or voice.dgagent.provider; empty when none does",
													},
												},
											},
										},
									},
								}},
							},
						},
						"501": map[string]any{
							"description": "This build checks no providers",
						},
					},
				},
			},
			"/api/tts/speak": map[string]any{
				"post": map[string]any{
					"summary":     "Speak text through the speaker",
					"description": "Speaks text with the text-to-speech role (tts.model), in voice or tts.voice. It waits in its queue while a voice exchange holds the speaker, and answers once the speech is synthesized, with the model and voice that spoke in the X-Ghost-Wispr-TTS-Model and X-Ghost-Wispr-TTS-Voice headers.",
					"tags":        []string{"tts"},
					"security":    []map[string][]string{{"basicAuth": {}}},
					"requestBody": map[string]any{
						"required": true,
						"content": map[string]any{
							"application/json": map[string]any{"schema": map[string]any{
								"type":     "object",
								"required": []string{"text"},
								"properties": map[string]any{
									"text":  map[string]any{"type": "string", "description": "What to say"},
									"voice": map[string]any{"type": "string", "description": "The voice, as the tts model names it; absent is tts.voice"},
								},
							}},
						},
					},
					"responses": map[string]any{
						"202": map[string]any{
							"description": "Synthesized and queued: {id, status}; GET /api/tts/status/{id} follows it",
							"headers": map[string]any{
								"X-Ghost-Wispr-TTS-Model": map[string]any{"schema": map[string]any{"type": "string"}, "description": "The model that spoke"},
								"X-Ghost-Wispr-TTS-Voice": map[string]any{"schema": map[string]any{"type": "string"}, "description": "The voice that spoke"},
							},
						},
						"400": map[string]any{"description": "No text, or a body that is not JSON"},
						"429": map[string]any{"description": "The queue is full, or too many requests"},
						"500": map[string]any{"description": "The speech could not be queued, or its synthesis could not be waited for"},
						"501": map[string]any{"description": "Text-to-speech is not configured: no tts.model"},
						"502": map[string]any{"description": "The provider failed to synthesize the speech"},
						"503": map[string]any{"description": "No speaker, or the speaker did not play the synthesized speech"},
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
							"description": "Log level filter: debug, info, warn or error, in any case. ALL, or no level, returns every level",
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
		"webhooks": map[string]any{
			"envoyEvent": map[string]any{
				"post": map[string]any{
					"summary":     "Session event published to the Envoy hub",
					"description": "With envoy_enabled, Ghost Wispr publishes session_started, session_ended and summary_ready to the Envoy hub's listener at envoy_hub_url, POST /v1/messages/publish, over the tailnet with no credential. Envoy stores each under notifications.ghostwispr.<session>.{session.started,session.ended,summary.ready}. No event carries a session's title, summary or transcript: Envoy keeps every event for 72 hours and no deletion can recall it. An event says which session it is, when it ran, and where to read its current text: session_url on this unit, and the session's Drive folder. Every send of one event carries the same idempotency_key, so Envoy stores and delivers a resend once.",
					// The publish carries no credential: the hub's listener takes it over the tailnet.
					"security": []map[string][]string{},
					"requestBody": map[string]any{
						"required": true,
						"content": map[string]any{
							"application/json": map[string]any{
								"schema": map[string]any{
									"type":     "object",
									"required": []string{"source", "topic", "message", "payload", "idempotency_key"},
									"properties": map[string]any{
										"source":          map[string]any{"type": "string", "enum": []string{"ghostwispr"}},
										"topic":           map[string]any{"type": "string", "description": "notifications.ghostwispr.<session>.<event>, the session id's . * > / and whitespace written as _"},
										"message":         map[string]any{"type": "string", "description": "ghostwispr <event> for session <session>; Envoy's payload_summary"},
										"payload":         map[string]any{"type": "string", "contentMediaType": "application/json", "contentSchema": envoyPayloadSchema},
										"idempotency_key": map[string]any{"type": "string", "format": "uuid", "description": "The name-based UUID of the event's delivery id: <session>.<event>, and .<version> for summary_ready"},
									},
								},
							},
						},
					},
					"responses": map[string]any{
						"200": map[string]any{"description": "Envoy stored the event, or already held it"},
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
