# In-App Config Panel Design

**Date:** 2026-02-28
**Issue:** #7

## Goal

Expose a settings panel in the web UI for runtime tuning of ghost-wispr configuration. Primary use case is ongoing adjustment between meetings (presets, silence timeout, transcription params), with first-time setup (GDrive, API keys) as a secondary benefit.

## Approach

**Config-in-Memory with YAML/.env Write-Back.** A `ConfigStore` wraps the `Config` struct with a mutex, validates mutations, writes non-secret settings to `ghost-wispr.yaml` and secrets to `.env`, and notifies components via onChange callbacks.

Rejected alternatives:
- **SQLite as config store**: overengineered for ~10 settings, creates two sources of truth
- **Hybrid (YAML + SQLite for presets)**: unnecessary split given low preset volume

## Settings Surface

| Setting | Priority | Hot-Reload Complexity |
|---------|----------|-----------------------|
| Summarization presets (full CRUD + test) | High | Low — read per-request |
| Silence timeout | High | Low — update Detector |
| Transcription tuning (endpointing, utterance_end_ms) | Medium | Medium — Deepgram reconnect |
| Google Drive setup (folder ID, credentials) | Medium | Medium — Syncer recreation |
| Default summarization model | Low | Low — read per-request |
| API keys (Deepgram, OpenAI, Anthropic, Gemini) | Low | Low — update map |
| Mic volume/gain | Nice-to-have | Unknown — PortAudio research needed |

## Architecture

### Runtime Config Layer

New `ConfigStore` in `internal/config/`:

```go
type Store struct {
    mu       sync.RWMutex
    cfg      Config
    path     string          // YAML file path for write-back
    envPath  string          // .env file path for secrets
    onChange []func(Config)  // notification callbacks
}

func NewStore(path string) (*Store, []string, error)
func (s *Store) Get() Config                        // RLock, return copy
func (s *Store) Update(fn func(*Config)) error      // Lock, mutate, validate, write, notify
func (s *Store) OnChange(fn func(Config))           // Register callback
```

Key properties:
- `Get()` returns a value copy — no partial reads
- `Update()` validates before committing; rolls back on failure
- YAML write-back uses `yaml.Marshal` (comments lost on first write — acceptable)
- Secrets write to `.env` file, never to YAML
- Secret precedence: environment variable > `.env` file > empty

### Changes to `main.go`

Replace `cfg, warnings, err := config.Load(configPath)` with:
```go
store, warnings, err := config.NewStore(configPath)
cfg := store.Get()  // for initial setup
```

Components register onChange callbacks or call `store.Get()` at use-time.

## API Endpoints

Three endpoints:

```
GET   /api/config                       → full config (keys show has/hasn't, not values)
PATCH /api/config                       → JSON Merge Patch (RFC 7386)
POST  /api/config/presets/{name}/test   → test preset against a past session
```

### GET /api/config response

```json
{
  "silence_timeout": "30s",
  "summarization": {
    "model": "anthropic/claude-sonnet-4-6-20250217",
    "base_url": "",
    "presets": {
      "default": {
        "description": "General-purpose meeting summary...",
        "system_prompt": "Summarize the following...",
        "user_template": "{{transcript}}",
        "model": ""
      }
    }
  },
  "transcription": {
    "endpointing": "400",
    "utterance_end_ms": "1000"
  },
  "gdrive": {
    "folder_id": "",
    "has_credentials": false
  },
  "api_keys": {
    "deepgram": true,
    "openai": false,
    "anthropic": true,
    "gemini": false
  }
}
```

### PATCH /api/config examples

```json
// Update silence timeout
{"silence_timeout": "45s"}

// Add/update a preset
{"summarization": {"presets": {"standup": {"description": "...", "system_prompt": "...", "user_template": "{{transcript}}"}}}}

// Delete a preset (null = remove per RFC 7386)
{"summarization": {"presets": {"old-one": null}}}

// Set an API key
{"api_keys": {"openai": "sk-..."}}

// Set GDrive config (credentials as base64)
{"gdrive": {"folder_id": "abc123", "credentials_base64": "eyJ..."}}
```

Validation runs before applying. On failure: 400 with field-level errors. No partial application.

### POST /api/config/presets/{name}/test

Request: `{"session_id": "20260228143000"}`
Response: `{"summary": "## Meeting Summary\n...", "error": ""}`

Synchronous (5-30s). No persistence — results are ephemeral for the user to evaluate.

## Component Reactions

onChange callbacks registered in `main.go`:

| Change | Reaction |
|--------|----------|
| Silence timeout | `detector.SetTimeout(d)` — new method |
| Presets / model / base_url | No-op — read from config per-request |
| API keys | Update `apiKeys` map (mutex-protected) |
| Transcription tuning | Reconnect Deepgram WebSocket |
| GDrive config | Recreate `Syncer` |

### Deepgram Reconnect

The most involved reaction. Steps:
1. Stop current `dgClient`
2. Create new `client.NewWSUsingCallback()` with updated `LiveTranscriptionOptions`
3. Connect
4. Swap the writer (mic stream continues uninterrupted)

Brief transcription gap (~1-2s). UI shows "Reconnecting transcription..." indicator.

## Frontend UI

### Navigation

New `/settings` route via a `currentView` state variable in `App.svelte` (no router library needed). Gear icon in the header toggles between main view and settings.

### Settings Page Layout

Three collapsible sections:

**1. Summarization Presets**
- List of preset cards (name + description)
- Click to expand: full editor with name, description, system_prompt (textarea), user_template (textarea), model override (input)
- "Add Preset" button
- Delete button per preset (disabled for "default")
- "Test" button: modal with session picker dropdown, "Run" button, spinner, summary result display

**2. General Settings**
- Silence timeout: input with Go duration format validation
- Default summarization model: input with `provider/model` hint
- Transcription: endpointing (ms), utterance_end_ms (ms) with reconnect warning

**3. Integrations**
- API keys: one row per provider, green/gray dot for status, masked password input, save per key
- Google Drive: folder ID input, credential file upload, enable/disable toggle, sync health indicator

### State Management

- New `configState` in `config.svelte.ts`, separate from `appState`
- Fetches on mount from `GET /api/config`
- Optimistic updates on save, revert on error
- Toast/banner for save success/failure

## Validation Rules

| Field | Rule |
|-------|------|
| silence_timeout | Must parse as Go `time.Duration` |
| summarization.model | Must be `provider/model_name` format |
| Preset system_prompt | Required, non-empty |
| Preset user_template | Required, non-empty |
| API keys | Non-empty string |
| GDrive credentials | Valid JSON with service account fields |

## Testing

- **ConfigStore**: unit tests for load, update, validate, YAML write-back, .env write-back
- **PATCH endpoint**: integration tests for various payloads, validation failures, partial updates
- **Deepgram reconnect**: test for writer swap with mock client
- **Frontend**: component tests for settings form (Vitest + testing-library), optimistic update + revert
