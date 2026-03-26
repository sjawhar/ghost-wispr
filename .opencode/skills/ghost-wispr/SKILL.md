---
name: ghost-wispr
description: Use when searching meeting transcripts, recalling past conversations, looking up what someone said, checking session history, or managing recordings on Ghost Wispr.
---

# Ghost Wispr

Always-on transcription appliance. Query it as a knowledge base via REST.

**Host**: `http://localhost:8080` (default). Override with `GHOST_WISPR_HOST` env var if remote.

**Full API reference**: `curl -s $GHOST_WISPR_HOST/api/openapi.json | jq`

## Workflows

### Find a past conversation

```bash
# Keyword search
curl -s "$GHOST_WISPR_HOST/api/search?q=entry+point+script" | jq

# Narrow by date, preset, or speaker
curl -s "$GHOST_WISPR_HOST/api/search?q=auth&date_from=2026-03-01T00:00:00Z&speaker=Ben" | jq

# Fuzzy/semantic search (finds related concepts even without exact words)
curl -s "$GHOST_WISPR_HOST/api/search/semantic?q=deployment+process" | jq
```

### Get context around a mention

```bash
# 5 minutes of conversation around a match
curl -s "$GHOST_WISPR_HOST/api/sessions/{id}/context?q=entry+point&seconds=300" | jq
```

### What did a specific person say?

```bash
# Filter search results by speaker name or index
curl -s "$GHOST_WISPR_HOST/api/search?q=budget&speaker=Ben" | jq

# Get one speaker's segments from a session
curl -s "$GHOST_WISPR_HOST/api/sessions/{id}/segments?speaker=Ben" | jq
```

### Cross-session queries

```bash
# How many standups this month?
curl -s "$GHOST_WISPR_HOST/api/sessions/aggregate?preset=standup&date_from=2026-03-01T00:00:00Z&group_by=date" | jq

# All sessions grouped by type
curl -s "$GHOST_WISPR_HOST/api/sessions/aggregate?group_by=preset" | jq
```

### What happened since I last checked?

```bash
# Poll for new events
curl -s "$GHOST_WISPR_HOST/api/events" | jq

# Continue from last cursor
curl -s "$GHOST_WISPR_HOST/api/events?cursor=42&types=session_ended,summary_ready" | jq
```

### Manage recordings

```bash
curl -s -X POST "$GHOST_WISPR_HOST/api/sessions/start"
curl -s -X POST "$GHOST_WISPR_HOST/api/pause"
curl -s -X POST "$GHOST_WISPR_HOST/api/resume"
curl -s -X POST "$GHOST_WISPR_HOST/api/sessions/current/stop"
```

### Browse sessions

```bash
# Available dates
curl -s "$GHOST_WISPR_HOST/api/dates" | jq

# Sessions for a date
curl -s "$GHOST_WISPR_HOST/api/sessions?date=2026-03-25" | jq

# Full session with transcript segments
curl -s "$GHOST_WISPR_HOST/api/sessions/{id}" | jq
```

## Quick Reference

| Action | Endpoint |
|--------|----------|
| Keyword search | `GET /api/search?q=TERM` |
| Semantic search | `GET /api/search/semantic?q=TERM` |
| Context window | `GET /api/sessions/{id}/context?q=TERM` |
| Speaker filter | `?speaker=NAME` on search or segments |
| Aggregate | `GET /api/sessions/aggregate?group_by=date\|preset` |
| Event polling | `GET /api/events?cursor=N` |
| Session list | `GET /api/sessions?date=YYYY-MM-DD` |
| Session detail | `GET /api/sessions/{id}` |
| Start/stop | `POST /api/sessions/start`, `POST /api/sessions/current/stop` |
| Resummarize | `POST /api/sessions/{id}/resummarize` |
| Config | `GET /api/config`, `PATCH /api/config` |
| OpenAPI spec | `GET /api/openapi.json` |

## Notes

- Dates use RFC3339 format: `2026-03-25T00:00:00Z`
- Speaker filter accepts names (`Ben`, case-insensitive) or indices (`0`, `1`)
- Semantic search returns HTTP 501 if no embedding provider configured — fall back to keyword search
- Event polling uses cursor-based pagination: pass `next_cursor` from previous response
- Auth is disabled by default. If `GHOST_WISPR_AUTH_TOKEN` is set on the server, add `-u "ghost-wispr:TOKEN"` to requests.
