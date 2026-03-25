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
