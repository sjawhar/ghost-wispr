---
name: ghost-wispr
description: Use when searching meeting transcripts, recalling past conversations, looking up what someone said, checking session history, or managing recordings on Ghost Wispr.
---

# Ghost Wispr

Always-on transcription appliance. Query it as a knowledge base via REST.

**Host**: !`echo ${GHOST_WISPR_HOST:-http://localhost:8080}`

## Search Strategy

Transcripts are spoken language, not written text. People say "we gotta ship this" not "deployment timeline." Search for how things are *said*, not how they'd be *written*. Names, technical terms, and unique phrases are the highest-signal search terms.

### Which search to use

- **Semantic search** (`/api/search/semantic`): Start here for topical or conceptual queries — "what did we discuss about priorities" or "the conversation about deployment." Finds related concepts even without exact word matches. Accepts a `limit` parameter (default 10).
- **Keyword search** (`/api/search`): Use for specific phrases, names, or technical terms — "Ben said something about Kubernetes" or exact quotes. Supports `speaker` and `preset` filtering that semantic search does not.
- **Context window** (`/api/sessions/{id}/context`): After finding a match, expand to get the surrounding conversation (default 5 minutes, configurable via `seconds` param).

### Recommended workflow

1. Try semantic search first for the concept
2. If 501 (no embeddings configured) or empty results, fall back to keyword search
3. Try 2-3 keyword variations — think spoken forms, synonyms, technical jargon, and casual equivalents. If the user describes something conceptually ("browning food"), also try the technical term ("Maillard"). If they use jargon, also try the plain-English version.
4. Narrow with `date_from`/`date_to` if you know roughly when
5. Narrow with `speaker=NAME` if you know who said it
6. Once you find the session, use the context endpoint to get the full discussion

### Pattern-specific workflow

- **Exact notes / exact quote / named person**: Do **keyword search first**, not just semantic. Search the person's name plus the concept (`Peter blocked`, `Ben priorities`, `Sammy merge`). Before concluding, verify the transcript actually contains the target phrase or recommendation.
- **Verification for exact-notes lookups**: Do not summarize from a plausible session title or fuzzy match alone. Quote the matching transcript line (or refined/canonical transcript snippet) first, then explain it.
- **Named person + exact concept**: Treat speaker filters as optional refinement, not ground truth. Speaker metadata can be missing or wrong. First find the session that matches the **conceptual condition** (`blocked by` + `still open`, `lead the investigation`, `focus on trying to merge`), then use speaker hints only if they help confirm the source.
- **Day audit / "what decisions happened on DATE"**: Start with `GET /api/sessions?date=YYYY-MM-DD`, then prioritize longer sessions and use search to narrow. This is a bounded review task, not a single-hit lookup.
- **Empty `segments` on session detail**: `GET /api/sessions/{id}` can still be useful even when `segments` is empty. Check `session.refined_transcript` and `session.canonical_transcript` before giving up.

### Common pitfalls

- **Too-specific compounds**: `priority+algorithm` matches almost nothing. Start with just `priority` or `P0`.
- **Written vs. spoken**: People say "gonna" not "going to", "standup" not "daily synchronization meeting."
- **Single words too broad**: `meeting` alone returns everything. Pair with date or speaker filters.
- **Give up too early**: Try at least 3 keyword variations before concluding something isn't there.
- **Only trying one register**: If you're searching for a concept, try both the casual way someone would say it AND the technical term. A conversation about "making food brown" will match on "Maillard" — and vice versa.
- **Semantic search returning empty**: This is normal for some topics even when embeddings are configured. It doesn't mean the conversation doesn't exist — just switch to keyword search.
- **Trusting the first plausible hit**: If the prompt asks for exact notes from a specific person, confirm the right speaker or exact phrase is present before summarizing.
- **Over-filtering by speaker**: Speaker names and speaker IDs are not always populated. If a speaker filter removes the strongest conceptual hit, inspect the hit anyway.
- **Relying only on `segments`**: Some sessions have useful canonical/refined transcript text even when segment arrays are empty.

## Workflows

### Find a past conversation

```bash
# Step 1: Try semantic search for the concept
curl -s "$GHOST_WISPR_HOST/api/search/semantic?q=discussion+about+deployment+process" | jq

# Step 2: If 501 or poor results, try keyword variations
curl -s "$GHOST_WISPR_HOST/api/search?q=deploy" | jq
curl -s "$GHOST_WISPR_HOST/api/search?q=shipping+to+prod" | jq
curl -s "$GHOST_WISPR_HOST/api/search?q=release+process" | jq

# Step 3: Narrow by date, preset, or speaker
curl -s "$GHOST_WISPR_HOST/api/search?q=deploy&date_from=2026-03-01T00:00:00Z&speaker=Ben" | jq

# Step 4: Get context around the match
curl -s "$GHOST_WISPR_HOST/api/sessions/{id}/context?q=deploy&seconds=300" | jq
```

### Find exact notes about a precise rule or condition

```bash
# Start with the precise condition, not just the person's name
curl -s "$GHOST_WISPR_HOST/api/search?q=blocked+by+issue+still+open" | jq
curl -s "$GHOST_WISPR_HOST/api/search?q=blocked+by+still+open" | jq
curl -s "$GHOST_WISPR_HOST/api/search?q=unblocked" | jq

# Only then add person-specific refinements if needed
curl -s "$GHOST_WISPR_HOST/api/search?q=Peter+blocked" | jq

# Verify the strongest conceptual hit directly
curl -s "$GHOST_WISPR_HOST/api/sessions/{id}" | jq '{title: .session.title, refined: .session.refined_transcript, canonical: .session.canonical_transcript}'
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

### Audit one day for decisions or action items

```bash
# Step 1: List all sessions for the day
curl -s "$GHOST_WISPR_HOST/api/sessions?date=2026-03-26" | jq '[.[] | {id, started_at, ended_at, duration_seconds}]'

# Step 2: Search within the same day for names and decision terms
curl -s "$GHOST_WISPR_HOST/api/search?q=Sammy&date_from=2026-03-26T00:00:00Z&date_to=2026-03-27T00:00:00Z" | jq
curl -s "$GHOST_WISPR_HOST/api/search?q=decision&date_from=2026-03-26T00:00:00Z&date_to=2026-03-27T00:00:00Z" | jq

# Step 3: Inspect promising sessions directly
curl -s "$GHOST_WISPR_HOST/api/sessions/{id}" | jq '{title: .session.title, refined: .session.refined_transcript, canonical: .session.canonical_transcript, segment_count: (.segments | length)}'
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
- Auth is disabled by default. If `GHOST_WISPR_AUTH_TOKEN` is set on the server, add `-u "ghost-wispr:$GHOST_WISPR_AUTH_TOKEN"` to requests.
