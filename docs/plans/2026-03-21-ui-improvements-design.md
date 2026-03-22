# UI Improvements Design

**Date**: 2026-03-21
**Status**: Approved
**Priority order**: Reload persistence → Summary preview → Empty session cleanup → Session merge

## Problem Statement

The Ghost Wispr web UI has four usability issues:

1. Reloading the page clears the live transcription panel — no way to recover an active session's transcript.
2. Collapsed session cards show raw markdown (`## headers`) in the summary preview instead of formatted text.
3. Many short/empty sessions clutter the history list with no useful content.
4. Silence detection splits real meetings into multiple short sessions with no way to recombine them.

## Design

### 1. Reload Persistence

**Goal**: After a page reload, restore the live transcription panel if a session is actively recording.

**Backend changes** (`internal/server/api.go`):
- Extend `GET /api/status` response to include:
  - `active_session_id` (string, empty if no active session)
  - `active_session_started_at` (ISO 8601 timestamp, empty if no active session)
- Wire through ControlHooks to expose the session Manager's current session state.

**Frontend changes** (`web/src/`):
- Update `StatusResponse` type in `types.ts` to include the new fields.
- In `App.svelte` bootstrap: if `status.active_session_id` is non-empty, call `fetchSession(id)` to get the session's segments, then populate `appState.liveSegments` and set `activeSessionId` / `activeSessionStartedAt`.
- WebSocket continues streaming new segments on top of restored ones.

**Edge case**: If the session ends between status fetch and session fetch, the segments still display correctly and the next `session_ended` WS event clears the state normally.

### 2. Markdown Summary Preview

**Goal**: Collapsed session cards render the summary as formatted markdown instead of raw text.

**Frontend changes** (`web/src/components/SessionCard.svelte`):
- Replace the plain-text `summaryPreview()` + `<p>` with a truncated `<Markdown>` render.
- Use a CSS-constrained container: `max-height` (~3.5em) + `overflow: hidden` + `mask-image: linear-gradient(...)` fade at the bottom.
- Reuses existing `@humanspeak/svelte-markdown` component and `prose` class.
- Remove the `summaryPreview()` function entirely.

**CSS** (`web/src/app.css`):
- Add `.summary-preview-md` class with constrained height, hidden overflow, and gradient mask.

### 3. Empty Session Filtering + Delete

**Goal**: Hide junk sessions by default and let users permanently delete them.

#### Filtering (frontend only)

**Frontend changes** (`web/src/components/SessionList.svelte`):
- Filter out sessions where:
  - Duration < 2 minutes AND summary is empty/pending/failed
  - OR summary text is effectively boilerplate (< 50 characters of actual content after stripping markdown headers)
- Per date group, show a "N short sessions hidden" toggle to reveal filtered sessions.
- Hardcode sensible defaults; configurable threshold can come later.

#### Delete (backend + frontend)

**Backend changes** (`internal/server/api.go`):
- Add `DELETE /api/sessions/{id}` route.
- Call `store.DeleteSession(id)` (already exists in storage layer).
- Delete the audio file from disk if `session.AudioPath` is non-empty.

**Frontend changes** (`web/src/components/SessionCard.svelte`):
- Add delete button on session cards (visible on hover or in expanded view).
- Confirm before deleting ("Delete this session?" dialog).
- On success, remove from `appState.sessionsByDate`.

**Frontend changes** (`web/src/lib/api.ts`):
- Add `deleteSession(id)` function calling `DELETE /api/sessions/{id}`.

### 4. User-Initiated Session Merge

**Goal**: Let users manually select multiple sessions and merge them into one combined session.

#### Backend

**New endpoint**: `POST /api/sessions/merge`
- Request body: `{ "session_ids": ["id1", "id2", ...] }`
- Validation: all sessions must exist, be ended, and belong to the same date.
- Creates a new session spanning earliest `started_at` to latest `ended_at`.
- Reassigns all segments from source sessions to the merged session: `UPDATE segments SET session_id = ? WHERE session_id IN (...)`, preserving timestamp order.
- Stores a comma-separated list of source audio paths on the merged session (no audio concatenation — avoids ffmpeg dependency).
- Marks original sessions with `status = 'merged'` and a new `merged_into` column referencing the new session ID.
- Triggers summary generation on the merged session.

**Schema changes** (`internal/storage/sqlite.go`):
- Add migration: `ALTER TABLE sessions ADD COLUMN merged_into TEXT NOT NULL DEFAULT ''`
- Add `MergeSessions(newID string, sourceIDs []string)` method.
- `GetSessionsByDate` already filters `status != 'discarded'` — extend to also filter `status != 'merged'`.

#### Frontend

**Multi-select mode** (`web/src/components/SessionList.svelte` + `SessionCard.svelte`):
- Add a "Select" toggle button to enter multi-select mode.
- Checkboxes appear on each session card.
- When 2+ sessions selected, show a "Merge" action button.
- On merge success, refresh the session list for that date.

**Audio playback** (`web/src/components/AudioPlayer.svelte`):
- Support comma-separated audio paths: play files in sequence with visual dividers.

**API** (`web/src/lib/api.ts`):
- Add `mergeSessions(sessionIds: string[])` function calling `POST /api/sessions/merge`.

## Non-Goals

- Automatic session merging (user-initiated only).
- Audio file concatenation (sequential playback of separate files instead).
- Configurable filter thresholds in settings (hardcode first, iterate later).
- localStorage/IndexedDB caching of session data (backend-informed restore is sufficient).
