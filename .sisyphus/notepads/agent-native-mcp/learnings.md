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
