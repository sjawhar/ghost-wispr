# Learnings — agent-native-mcp

## Project Context
- Ghost Wispr: Go binary, Svelte 5 frontend, Deepgram STT, SQLite (modernc.org/sqlite — pure Go, NO C extensions)
- Multi-provider LLM: Anthropic, OpenAI, Gemini via internal/llm/ abstraction
- Auth: HTTP Basic auth, token from GHOST_WISPR_AUTH_TOKEN env var
- Version control: jj (not git) — use `jj describe -m "msg"` then `jj new` for commits

## Key Architectural Decisions
- REST-first (no MCP) — agents use curl/HTTP, guided by skill + OpenAPI spec
- App-side cosine similarity for semantic search (no sqlite-vec, no C extensions)
- Embeddings stored as BLOB in SQLite embeddings table
- Event queue: new `mcp_events` SQLite table, hooked into existing Hub broadcasts
- Speaker names: LLM extraction during summarization, stored as JSON in speaker_names column
- Meeting types: use summary_preset column as proxy

## Existing Patterns to Follow
- internal/llm/llm.go — Client interface + NewClient factory (follow for embedding client)
- internal/storage/sqlite.go — ensureSchema() for migrations, WAL mode
- internal/server/api.go — handler registration pattern, JSON response helpers
- internal/server/hub.go — Hub broadcast methods (hook event queue here)
- internal/gc/gc.go — GC cycle (hook event purge here)
- internal/summary/summarizer.go — structuredSchema() for LLM JSON extraction

## 2026-03-25 — Ghost Wispr REST skill authoring notes
- Skill style in `~/.config/opencode/skills/sjawhar/*/SKILL.md` is concise frontmatter + markdown headings + quick-start-first examples.
- Endpoint inventory should come from `internal/server/api.go` plus delegated route helpers (`registerLogRoutes` -> `/api/logs`).
- Auth source of truth in `internal/server/auth.go`: Basic auth expects `ghost-wispr:<token>` (token from `GHOST_WISPR_AUTH_TOKEN`).
- Data model fields for skill docs should mirror `storage.Session`, `storage.SearchResult`, `segments` schema, and `config.Preset`.
- For agent-facing curl snippets, keep placeholders stable: `$GHOST_WISPR_HOST` and `$GHOST_WISPR_TOKEN`.

## 2026-03-25 — Task 2: Filtered Search Implementation

### Implementation Pattern
- **SearchOptions struct**: Optional filter fields (DateFrom, DateTo, Preset) as empty strings
- **Dynamic SQL**: Build WHERE clauses conditionally using strings.Join() for optional filters
- **Backward compatibility**: Empty SearchOptions{} produces identical behavior to original code
- **Date format**: RFC3339Nano for timestamp comparisons in SQL

### Key Decisions
- Filters are optional query parameters: `?date_from=`, `?date_to=`, `?preset=`
- Date filtering uses `sessions.started_at` column (RFC3339Nano format)
- Preset filtering uses exact match on `sessions.summary_preset` column
- All filters can be combined: `?q=term&date_from=X&date_to=Y&preset=Z`

### Testing Approach
- TDD: Write failing tests first, then implement
- Storage layer: 4 new tests (date_from, date_to, preset, combined filters)
- API handler: 3 new tests (with filters, with preset, backward compat)
- All existing tests updated to pass empty SearchOptions{}
- No regressions: full test suite passes

### Code Patterns
- SessionStore interface updated: `Search(query string, opts SearchOptions) ([]SearchResult, error)`
- Handler parses query params and creates SearchOptions struct
- SQL query built with fmt.Sprintf() using dynamic WHERE clause
- Variable naming: renamed `query` to `sqlQuery` to avoid shadowing parameter

### Testing Stubs
- Added `healthCheckerStub` type to api_test.go for test support
- Updated `apiStoreStub.Search()` signature to match interface

## 2026-03-25 — Task 3: Context Window Endpoint

### Implementation Pattern
- **GetSegmentsInTimeRange**: SQL query on `segments` table with `start_time >= ? AND end_time <= ?`, ordered by `start_time`
- **Context handler**: Two-phase — first load all segments to find text match (case-insensitive), then use SQL range query for the window
- **No full transcript in memory**: Only loads all segments for matching; context window uses efficient SQL range query

### Key Decisions
- `q` param is required (400 if missing), `seconds` defaults to 300
- Match: `strings.Contains(strings.ToLower(seg.Text), strings.ToLower(q))` — first segment wins
- Window: `[matchTime - seconds/2, matchTime + seconds/2]` — symmetric around match
- 404 if session not found, 422 if no segment matches the query text
- Added `strconv` import to api.go for ParseFloat on seconds param

### Testing
- Storage: 1 test covering multiple range scenarios (partial, empty, full, ordering)
- Handler: 5 tests (success, default seconds, session not found, no match, missing query)
- `apiStoreStub.GetSegmentsInTimeRange` filters in-memory for test simplicity
- Full suite: 14 packages, 0 regressions

### Interface Pattern
- `SessionStore` interface extended with `GetSegmentsInTimeRange(sessionID string, startTime, endTime float64) ([]transcribe.Segment, error)`
- Route registered after `GET /api/sessions/{id}/audio` in registerAPIRoutes()

## 2026-03-25 — Task 4: Cross-Session Aggregation Endpoint

### Implementation Pattern
- **AggregateOptions struct**: DateFrom, DateTo, Preset (filter), GroupBy ("date"|"preset")
- **AggregateResult struct**: SessionCount int, Groups []AggregateGroup
- **AggregateGroup struct**: Key string, Count int, Sessions []SessionSummary
- **SessionSummary struct**: Lightweight — ID, Title, StartedAt, SummaryPreset (not full Session)
- **SQL approach**: Query sessions table with optional WHERE filters, group in Go (not SQL GROUP BY)
- **Group ordering**: Preserves SQL ORDER BY started_at DESC order — first-seen key wins

### Key Decisions
- Route `GET /api/sessions/aggregate` registered BEFORE `GET /api/sessions/{id}` to avoid path conflict
- GroupBy defaults to "date" if omitted; validates against "date"|"preset" only
- Discarded and merged sessions excluded via `status NOT IN ('discarded', 'merged')`
- Handler validates group_by and returns 400 for invalid values

### Testing Approach
- TDD: Storage tests first (5 subtests), then API endpoint tests (3 tests)
- Storage tests: no filters, group by preset, date range filter, preset filter, field population
- API tests: success response structure, query param forwarding, invalid group_by rejection
- Full suite: 14 packages, 0 regressions

### Interface Pattern
- `SessionStore` interface extended with `AggregateSessions(opts storage.AggregateOptions) (storage.AggregateResult, error)`
- `apiStoreStub` extended with `aggregateResult *storage.AggregateResult` and `aggregateErr error` fields

## 2026-03-25 — Task 5: OpenAPI 3.1 Spec Endpoint

### Implementation Pattern
- **Static Go struct approach**: `OpenAPISpec()` returns `map[string]any` with full OpenAPI 3.1.0 structure
- **No code generation**: Manually defined spec covers all 32 registered routes (existing + Wave 1 additions)
- **Endpoint registration**: `GET /api/openapi.json` registered in `registerAPIRoutes()` after version endpoint
- **Handler**: Simple `writeJSON(w, http.StatusOK, OpenAPISpec())` with automatic JSON encoding

### Key Decisions
- **Spec location**: `internal/server/openapi.go` — dedicated file for spec definition
- **Route coverage**: All 32 paths documented including health checks, sessions CRUD, context, aggregate, config, logs, diagnostics
- **Security scheme**: `basicAuth` with HTTP Basic auth type, documented at top level and in components
- **No external dependencies**: Pure Go, no swaggo or oapi-codegen
- **Spec completeness**: Includes parameters, request bodies, response descriptions, tags, and security requirements

### Testing Approach
- TDD: Write failing test first (`TestOpenAPISpec`)
- Verify: OpenAPI 3.1.0 structure, info field, paths field, security schemes, expected paths present
- Full suite: 14 packages, 0 regressions

### Paths Documented (32 total)
- Health: `/healthz/live`, `/healthz/ready`
- System: `/api/version`, `/api/status`, `/api/logs`, `/api/openapi.json`
- Search: `/api/search` (with date_from, date_to, preset filters)
- Sessions: `/api/sessions`, `/api/sessions/aggregate`, `/api/sessions/{id}`, `/api/sessions/{id}/audio`, `/api/sessions/{id}/context`
- Session actions: `/api/sessions/{id}/resummarize`, `/api/sessions/{id}/retry-summary`, `/api/sessions/{id}/retry-sync`, `/api/sessions/{id}/retry-refinement`
- Session control: `/api/sessions/start`, `/api/sessions/current/stop`, `/api/sessions/{id}/stop`, `/api/sessions/merge`
- Control: `/api/pause`, `/api/resume`, `/api/session/end`
- Config: `/api/config`, `/api/presets`, `/api/config/presets/{name}/test`, `/api/config/presets/generate`, `/api/config/presets/refine`
- Dates: `/api/dates`
- Restore: `/api/restore/gdrive`
- Diagnostic: `/api/diagnostic/mic`
- Test: `/api/test/fault/deepgram-disconnect`
## 2026-03-25 — Task 6: Embedding Client Abstraction

### Implementation Pattern
- New `internal/embedding` package mirrors `internal/llm` style: `Client` interface + `ParseModel` + `NewClient` provider factory.
- Model format is `provider/model` and factory routes by provider (`openai`, `gemini`) with descriptive unknown-provider errors.
- Provider clients return `[][]float32` for batch input and preserve per-input ordering.

### Provider Notes
- OpenAI uses `go-openai` `CreateEmbeddings` with `EmbeddingRequestStrings` and wraps API failures as `openai embedding: ...`.
- Gemini uses `genai` `Models.EmbedContent` with a `[]*genai.Content` batch built via `NewContentFromText`.
- Both providers treat empty input as empty output and validate missing/partial embedding responses.

### Config Pattern
- Added `EmbeddingModel string` (`yaml:"embedding_model"`) to config with default empty string (feature disabled by default).
- Added env override `GHOST_WISPR_EMBEDDING_MODEL`.
- Validation now includes embedding model provider checks when `embedding_model` is configured.

### Testing Pattern
- TDD executed: created failing tests first, then implemented package to green.
- Required tests added: `TestEmbedReturnsVectors`, `TestUnknownProvider`, `TestBatchEmbedding` using mock clients (no external API calls).

## 2026-03-25 — Task 7: Embedding Storage Schema and CRUD

### Implementation Pattern
- **StoredEmbedding struct**: SessionID, ChunkIndex, Vector []float32, TextHash, Model, CreatedAt
- **Table schema**: embeddings(session_id, chunk_index, embedding BLOB, text_hash, model, created_at)
- **Primary key**: (session_id, chunk_index) — one embedding per chunk per session
- **Foreign key**: session_id references sessions(id) ON DELETE CASCADE

### Vector Serialization
- **Serialization**: `binary.Write(buf, binary.LittleEndian, vector)` → BLOB
- **Deserialization**: `binary.Read(buf, binary.LittleEndian, &f)` in loop until EOF
- **Imports**: Added `"bytes"` and `"encoding/binary"` to sqlite.go

### CRUD Methods
- **StoreEmbedding(sessionID, chunkIndex, vector, textHash, model)**: Insert with serialized vector
- **GetEmbeddings(sessionID)**: Query by session, deserialize vectors, return ordered by chunk_index
- **GetAllEmbeddings()**: Query all embeddings (for brute-force cosine similarity), ordered by session_id, chunk_index
- **DeleteEmbeddings(sessionID)**: Delete all embeddings for a session

### Testing Approach
- TDD: 3 tests written first, then implementation
- TestStoreEmbedding: Store + retrieve, verify all fields and vector values
- TestDeleteEmbeddings: Store multiple, delete, verify gone
- TestGetAllEmbeddings: Multiple sessions, verify cross-session retrieval
- Full suite: 14 packages, 0 regressions

### Migration Pattern
- Added to ensureSchema() after summary_requests table
- CREATE TABLE IF NOT EXISTS for idempotency
- Follows existing pattern: no ALTER TABLE needed for fresh installs

## 2026-03-25 — Task 8: Embedding Indexer Pipeline

### Implementation Pattern
- New `internal/embedding/indexer.go` owns chunking, hash-based idempotency, embedding generation, and persistence.
- `SplitIntoChunks(text, chunkSize)` uses word-based chunking with fixed overlap (50 words) and sane defaults (`chunkSize=500`).
- `IndexSession(ctx, sessionID, transcript)` computes per-chunk SHA-256 hashes and skips unchanged chunks by comparing against stored `text_hash`.

### Backfill Pattern
- Added `SQLiteStore.GetSessionsWithoutEmbeddings()` query for ended sessions with canonical transcripts and zero embedding rows.
- `BackfillMissing(ctx)` processes those sessions with bounded concurrency (semaphore via buffered channel, max 2 workers).
- Backfill loads canonical transcript per session and reuses `IndexSession` so idempotency and storage logic are centralized.

### Wiring Pattern
- `session.Manager` now supports optional `EmbeddingIndexer` via `SetIndexer`.
- After canonical transcript is available in `generateSummary`, manager triggers indexing as best-effort async work (logs errors, never fails session flow).
- `cmd/ghost-wispr/main.go` now creates indexer when `embedding_model` is configured, wires it into manager, and starts startup backfill goroutine.

### Storage Detail
- `StoreEmbedding` switched to SQLite UPSERT on `(session_id, chunk_index)` so changed chunks are updated in-place while unchanged chunks are skipped.

### Testing Pattern
- TDD flow used: added failing tests first (`TestSplitIntoChunks`, `TestIndexerOnSessionEnd`, `TestBackfillMissing`) then implemented to green.
- `TestIndexerOnSessionEnd` validates DB persistence plus idempotent no-op reindexing.
- `TestBackfillMissing` verifies only unindexed sessions are embedded during backfill.

## 2026-03-25 — Task 9: Semantic Search Endpoint

### Implementation Pattern
- Added `GET /api/search/semantic` route in `internal/server/api.go` before keyword search route.
- Handler flow: validate query params (`q`, optional `limit`, optional `date_from/date_to`) → embed query via `embedding.Client` → load all embeddings from store → cosine similarity against each vector → descending sort → top-K selection.
- Response shape implemented as `{ "results": [...] }` with `session_id`, `title`, `chunk_text`, `similarity`, and `chunk_index` fields.

### Wiring Pattern
- Extended `SessionStore` interface with `GetAllEmbeddings()` for semantic retrieval.
- Threaded `embedding.Client` through server setup by extending `HandlerWithLogger(..., embeddingClient embedding.Client, ...)` and passing it into `registerAPIRoutes(...)`.
- `cmd/ghost-wispr/main.go` now keeps a reusable `embeddingClient` variable and passes it to HTTP handler setup.

### Error/Edge Handling
- Returns `501` with `{ "error": "semantic search unavailable", "suggestion": "use /api/search for keyword search" }` when embedding client is nil.
- Returns `422` with `{ "error": "no embeddings indexed yet" }` when embeddings table is empty.
- Cosine similarity helper returns 0 for zero-norm or dimension-mismatch vectors.

### Testing Pattern
- TDD performed: introduced failing `TestSemanticSearch_*` tests first, then implemented endpoint and wiring.
- Added tests:
  - `TestSemanticSearch_Success`
  - `TestSemanticSearch_NoClient`
  - `TestSemanticSearch_NoEmbeddings`
- Verified no regressions with `go test ./... -v`.
