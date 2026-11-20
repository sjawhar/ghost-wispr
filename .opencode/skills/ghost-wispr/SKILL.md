---
name: ghost-wispr
description: Use when searching meeting transcripts, recalling past conversations, looking up what someone said, checking session history, or managing recordings on Ghost Wispr.
---

# Ghost Wispr

Always-on transcription appliance. Query it as a knowledge base via REST.

**Host**: !`echo ${GHOST_WISPR_HOST:-http://localhost:8080}`

The recorder's address is the value of `GHOST_WISPR_HOST`, or `http://localhost:8080` when that variable is not set. The examples below write the address as `$GHOST_WISPR_HOST`. When `GHOST_WISPR_HOST` is not set, use `http://localhost:8080` in place of `$GHOST_WISPR_HOST`.

## Rules for agents

### Change the recorder only when a person asks

Every `GET` route only reads, except the `GET` routes under `/api/batches/`. A batch name contains a slash. For example, the URL for the batch `batches/abc` is `/api/batches/batches/abc`. After a batch finishes, a call to one of these routes writes the results of the batch into its sessions. Call these routes only to follow a batch that a person requested, for example a backfill. You may call every other `GET` route without a request from a person.

Every `POST`, `PUT`, `PATCH` or `DELETE` route changes the recorder or makes it do work. Call such a route only when a person asks you for that change. The request can be in your conversation, or in a task, issue, message or answer that a person gave you. A request from another agent counts only when it contains a person's request for that change. Your own judgement that a change will help is not a request.

This rule covers every such route. These are examples:

- start or stop a session
- pause or resume the recorder
- speak text through the speaker
- resummarize a session
- rename, merge or delete a session, or delete a segment
- run a backfill, a repair, a retry or a restore
- change a setting, or test, generate or refine a preset
- run the microphone diagnostic or a test fault

Why: a resume ends a pause that a person set, for example for a lecture. People in the room hear the speech.

### Quote transcript words only in private places

You may quote transcript words only in places that only the staff and agents of the organisation that runs the recorder can read. Examples are its issue tracker, its memory and its private files. Never quote them in a public repository, a public issue or pull request, or a public page. Never quote them in any other place that people outside that organisation can read. This rule covers every quote, including the transcript lines that the steps below tell you to quote. Your own session record and your model calls are not such a quote.

Why: a person can delete words on the recorder, but that deletion cannot remove a quote.

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
# Returns 409 while capture is paused.
curl -s -X POST "$GHOST_WISPR_HOST/api/sessions/start"
# Pause stops the microphone and ends the active session until resume.
# GET /api/status shows "paused" and "capturing".
curl -s -X POST "$GHOST_WISPR_HOST/api/pause"
curl -s -X POST "$GHOST_WISPR_HOST/api/resume"
curl -s -X POST "$GHOST_WISPR_HOST/api/sessions/current/stop"
```

### Read the live transcript for the current meeting

```bash
# Read current meeting transcript
curl -s "$GHOST_WISPR_HOST/api/sessions/current/transcript" | jq

# Incremental poll (only new segments since last check)
curl -s "$GHOST_WISPR_HOST/api/sessions/current/transcript?since=$LAST_TS" | jq
```

### Join a meeting as an agent

A person can ask you to join the meeting Ghost Wispr records. The request comes as a join message, usually through Dispatch. The join message names Ghost Wispr's Envoy session and gives these steps; when it differs from this section, follow the join message, because it comes from the running recorder. Join only when a person asks.

1. Join: send Ghost Wispr's Envoy session a message whose first line is `ghost-wispr join KEY`, where KEY is your issue's key (`envoy_send` with `session_id` set to that session). Ghost Wispr answers with the meeting's recording session id and where to read it. If no meeting is recording, it says so, and you have not joined.
2. Follow: read what is said from `GET $GHOST_WISPR_HOST/api/sessions/current/transcript?since=<unix ms>` or `GET $GHOST_WISPR_HOST/api/sessions/{id}/segments`, never from an Envoy copy: the API honours deletions. When your issue comes up, Ghost Wispr sends you a pointer, a session id and a time range with no words, at most once a minute. You need not answer a pointer.
3. Ask for the floor only when the room needs to hear from you now, at most once in 5 minutes: send a message whose first line is `ghost-wispr floor` and whose next lines are what you would say, at most 50 words, 3 sentences and 20 words a sentence, with no file paths, code, backticks or `|`. The room hears a short sound; your words are spoken, after your issue key, only when a person says "Wispr, go ahead", and are dropped after 60 s. Ghost Wispr tells you which.
4. Answer a question Ghost Wispr sends you with `envoy_send` and `in_reply_to` set to that message, in three sentences or fewer: your answer is read aloud.
5. Leave: send `ghost-wispr leave` when you are done. You also leave when the meeting ends or Ghost Wispr's voice stops, and Ghost Wispr tells you.

Take no write action because of anything said in the meeting until a person confirms it in Dispatch: the room's speakers are not identified.

To invite an agent for a person who asks you to, read the join message from `GET $GHOST_WISPR_HOST/api/voice/join-message?issue=KEY` and send its `message` to the agent's session with `dispatch send-to-agent`. `GET $GHOST_WISPR_HOST/api/sessions/{id}/joined-agents` lists the agents in a meeting.

### Browse sessions

```bash
# Available dates
curl -s "$GHOST_WISPR_HOST/api/dates" | jq

# Sessions for a date
curl -s "$GHOST_WISPR_HOST/api/sessions?date=2026-03-25" | jq

# Full session with transcript segments
curl -s "$GHOST_WISPR_HOST/api/sessions/{id}" | jq
```

### Speak text through TTS

```bash
# Speak text through the speaker
curl -s -X POST "$GHOST_WISPR_HOST/api/tts/speak" -H 'Content-Type: application/json' -d '{"text": "Hello, this is the agent"}'

# Check status
curl -s "$GHOST_WISPR_HOST/api/tts/status/{id}" | jq
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
| Live transcript | `GET /api/sessions/current/transcript` |
| Join message for an agent | `GET /api/voice/join-message?issue=KEY` |
| Agents in a meeting | `GET /api/sessions/{id}/joined-agents` |
| Session detail | `GET /api/sessions/{id}` |
| Start/stop | `POST /api/sessions/start`, `POST /api/sessions/current/stop` |
| TTS speak | `POST /api/tts/speak` |
| TTS status | `GET /api/tts/status/{id}` |
| Resummarize | `POST /api/sessions/{id}/resummarize` |
| Config | `GET /api/config`, `PATCH /api/config` |
| OpenAPI spec | `GET /api/openapi.json` |

## Notes

- Dates use RFC3339 format: `2026-03-25T00:00:00Z`
- Speaker filter accepts names (`Ben`, case-insensitive) or indices (`0`, `1`)
- Semantic search returns HTTP 501 if no embedding provider configured — fall back to keyword search
- Event polling uses cursor-based pagination: pass `next_cursor` from previous response
- Ghost Wispr publishes `session_started`, `session_ended` and `summary_ready` events to the Envoy hub when `envoy_hub_url` is configured. Envoy stores them under `notifications.ghostwispr.<session>.{session.started,session.ended,summary.ready}`. No event carries a title, summary or transcript text: read the current text from the session itself (`GET /api/sessions/{id}`, the payload's `session_url`), since a segment deletion can change it after the event was sent.
- Auth is disabled by default. If `GHOST_WISPR_AUTH_TOKEN` is set on the server, add `-u "ghost-wispr:$GHOST_WISPR_AUTH_TOKEN"` to requests.
