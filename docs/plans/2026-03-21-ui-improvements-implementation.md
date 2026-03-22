# UI Improvements Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Fix four UI pain points: restore live transcription on page reload, render markdown in summary previews, filter/delete empty sessions, and allow user-initiated session merging.

**Architecture:** Each feature is a vertical slice through Go backend (API/storage) → TypeScript frontend (api/state/components). Features are independent and can be shipped incrementally. Priority order: reload persistence → summary preview → empty session cleanup → session merge.

**Tech Stack:** Go 1.25+ (backend), Svelte 5 + TypeScript (frontend), SQLite (storage), Vite (build), vitest (frontend tests), `go test` (backend tests).

**Design doc:** `docs/plans/2026-03-21-ui-improvements-design.md`

**Test commands:**
- Backend: `go test ./...`
- Frontend: `cd web && npm test`
- Dev server: `cd web && npm run dev` (proxies API to :8080)

---

## Feature 1: Reload Persistence

### Task 1.1: Expose active session in status endpoint (backend)

**Files:**
- Modify: `internal/server/api.go` (status handler, ~line 236)
- Modify: `internal/server/server.go` (ControlHooks struct)
- Modify: `internal/server/api_test.go` (add test)

**Step 1: Write the failing test**

Add a test to `internal/server/api_test.go` that verifies `GET /api/status` returns `active_session_id` and `active_session_started_at` fields. Set up ControlHooks with a mock `ActiveSession` callback that returns a session ID and timestamp.

```go
func TestStatusReturnsActiveSession(t *testing.T) {
    startedAt := time.Date(2026, 3, 21, 10, 0, 0, 0, time.UTC)
    hooks := &ControlHooks{
        IsPaused: func() bool { return false },
        Warnings: func() []string { return nil },
        ActiveSession: func() (string, time.Time) {
            return "20260321100000", startedAt
        },
    }
    // Create server, make GET /api/status request
    // Assert response includes active_session_id and active_session_started_at
}
```

**Step 2: Run test to verify it fails**

Run: `go test ./internal/server/ -run TestStatusReturnsActiveSession -v`
Expected: FAIL — `ActiveSession` field doesn't exist on ControlHooks yet.

**Step 3: Add ActiveSession to ControlHooks**

In `internal/server/server.go`, add to the `ControlHooks` struct:

```go
ActiveSession func() (string, time.Time) // Returns (sessionID, startedAt); empty string if no active session
```

**Step 4: Update status handler to include active session fields**

In `internal/server/api.go`, update the `GET /api/status` handler (~line 236):

```go
var activeSessionID string
var activeSessionStartedAt string
if controls.ActiveSession != nil {
    id, startedAt := controls.ActiveSession()
    activeSessionID = id
    if id != "" {
        activeSessionStartedAt = startedAt.UTC().Format(time.RFC3339Nano)
    }
}
writeJSON(w, http.StatusOK, map[string]any{
    "paused":                   paused,
    "warnings":                 warnings,
    "active_session_id":        activeSessionID,
    "active_session_started_at": activeSessionStartedAt,
})
```

**Step 5: Wire up ActiveSession in main.go**

In `cmd/ghost-wispr/main.go`, set the `ActiveSession` hook on the ControlHooks to call the session Manager's `currentSession()` method. The Manager needs a new exported method:

In `internal/session/manager.go`, add:

```go
func (m *Manager) ActiveSession() (string, time.Time) {
    m.mu.Lock()
    defer m.mu.Unlock()
    return m.currentSessionID, m.currentStartedAt
}
```

Then in `main.go`, wire it:

```go
controls.ActiveSession = sessionManager.ActiveSession
```

**Step 6: Run test to verify it passes**

Run: `go test ./internal/server/ -run TestStatusReturnsActiveSession -v`
Expected: PASS

**Step 7: Also verify the empty case**

Add a test where `ActiveSession` returns `("", time.Time{})` and verify the response has empty strings for both fields.

**Step 8: Run full backend tests**

Run: `go test ./...`
Expected: All PASS

**Step 9: Commit**

```
jj describe -m "feat: expose active session ID in status endpoint"
jj new
```

---

### Task 1.2: Restore live transcription on page reload (frontend)

**Files:**
- Modify: `web/src/lib/types.ts` (StatusResponse type)
- Modify: `web/src/App.svelte` (bootstrap function)
- Modify: `web/src/lib/state.svelte.ts` (if needed for setter)
- Test: `web/src/components/__tests__/` (existing test patterns)

**Step 1: Update StatusResponse type**

In `web/src/lib/types.ts`, update `StatusResponse`:

```typescript
export interface StatusResponse {
  paused: boolean
  warnings: string[]
  active_session_id: string
  active_session_started_at: string
}
```

**Step 2: Update bootstrap in App.svelte**

In `web/src/App.svelte`, in the `bootstrap` async function, after setting paused/warnings/dates/presets, add:

```typescript
// Restore active session if one exists
if (status.active_session_id) {
    appState.activeSessionId = status.active_session_id
    appState.activeSessionStartedAt = Date.parse(status.active_session_started_at)

    try {
        const detail = await fetchSession(status.active_session_id)
        setSessionDetail(detail)
        // Populate liveSegments from persisted segments
        appState.liveSegments = detail.segments.map((seg) => ({
            type: 'live_transcript' as const,
            version: 1,
            timestamp: seg.timestamp,
            speaker: seg.speaker,
            text: seg.text,
            start_time: seg.start_time,
            end_time: seg.end_time,
        }))
    } catch {
        // Session may have ended between status and detail fetch — that's OK
    }
}
```

**Step 3: Run frontend tests**

Run: `cd web && npm test`
Expected: Existing tests PASS (no breaking changes — we only added fields).

**Step 4: Manual verification**

With dev server running: start a session, see live transcription, reload page. The live panel should restore the existing transcript.

**Step 5: Commit**

```
jj describe -m "feat: restore live transcription on page reload"
jj new
```

---

## Feature 2: Markdown Summary Preview

### Task 2.1: Render markdown in collapsed session preview

**Files:**
- Modify: `web/src/components/SessionCard.svelte` (replace summaryPreview with Markdown render)
- Modify: `web/src/app.css` (add truncated preview styles)

**Step 1: Replace summaryPreview with truncated Markdown component**

In `web/src/components/SessionCard.svelte`:

1. Remove the `summaryPreview()` function (lines 86-92).
2. Replace the summary preview section (~lines 139-145):

Before:
```svelte
{#if session.summary_status === 'completed' && session.summary}
    <p class="summary-preview">{summaryPreview(session.summary)}</p>
```

After:
```svelte
{#if session.summary_status === 'completed' && session.summary}
    <div class="summary-preview-md prose">
        <Markdown source={session.summary} />
    </div>
```

**Step 2: Add CSS for truncated markdown preview**

In `web/src/app.css`, add after the existing `.summary-preview` styles:

```css
.summary-preview-md {
    margin: 0 0.85rem 0.7rem;
    max-height: 4em;
    overflow: hidden;
    position: relative;
    font-size: 0.88rem;
    line-height: 1.5;
    mask-image: linear-gradient(to bottom, black 50%, transparent 100%);
    -webkit-mask-image: linear-gradient(to bottom, black 50%, transparent 100%);
}

.summary-preview-md :global(h1),
.summary-preview-md :global(h2),
.summary-preview-md :global(h3) {
    margin: 0 0 0.2rem;
    font-size: 0.85rem;
    font-weight: 600;
}

.summary-preview-md :global(p) {
    margin: 0 0 0.2rem;
}

.summary-preview-md :global(ul),
.summary-preview-md :global(ol) {
    margin: 0;
    padding-left: 1.2rem;
}
```

**Step 3: Run frontend tests**

Run: `cd web && npm test`
Expected: PASS. If any tests assert on `summary-preview` class, update them to `summary-preview-md`.

**Step 4: Commit**

```
jj describe -m "feat: render markdown in collapsed session summary preview"
jj new
```

---

## Feature 3: Empty Session Filtering + Delete

### Task 3.1: Add delete session API endpoint (backend)

**Files:**
- Modify: `internal/server/api.go` (new DELETE route)
- Modify: `internal/server/api_test.go` (add tests)

**Step 1: Write failing tests**

Add tests for `DELETE /api/sessions/{id}`:
- Returns 204 on success
- Returns 404 for unknown session
- Returns 403 for invalid session ID
- Deletes audio file if present

**Step 2: Implement the endpoint**

In `internal/server/api.go`, in `registerAPIRoutes`, add:

```go
mux.HandleFunc("DELETE /api/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
    sessionID := r.PathValue("id")
    if !validSessionID(sessionID) {
        writeJSONError(w, http.StatusForbidden, "invalid session id")
        return
    }

    sessionData, err := store.GetSession(sessionID)
    if err != nil {
        status := http.StatusInternalServerError
        if errors.Is(err, os.ErrNotExist) || errors.Is(err, sql.ErrNoRows) {
            status = http.StatusNotFound
        }
        writeJSONError(w, status, fmt.Sprintf("get session: %v", err))
        return
    }

    // Delete audio file if it exists
    if sessionData.AudioPath != "" {
        _ = os.Remove(sessionData.AudioPath)
    }

    if err := store.DeleteSession(sessionID); err != nil {
        writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("delete session: %v", err))
        return
    }

    w.WriteHeader(http.StatusNoContent)
})
```

Note: `store.DeleteSession(id)` already exists in `internal/storage/sqlite.go`. The `SessionStore` interface in `api.go` needs to be extended to include it:

```go
type SessionStore interface {
    // ... existing methods ...
    DeleteSession(id string) error
}
```

**Step 3: Run tests**

Run: `go test ./internal/server/ -v`
Expected: PASS

**Step 4: Run full backend tests**

Run: `go test ./...`
Expected: All PASS

**Step 5: Commit**

```
jj describe -m "feat: add DELETE /api/sessions/{id} endpoint"
jj new
```

---

### Task 3.2: Add delete and filter functionality (frontend)

**Files:**
- Modify: `web/src/lib/api.ts` (add deleteSession)
- Modify: `web/src/lib/state.svelte.ts` (add removeSession helper)
- Modify: `web/src/components/SessionCard.svelte` (add delete button)
- Modify: `web/src/components/SessionList.svelte` (add filtering logic + hidden count toggle)
- Modify: `web/src/app.css` (styles for delete button, filter toggle)

**Step 1: Add deleteSession API function**

In `web/src/lib/api.ts`:

```typescript
export function deleteSession(id: string): Promise<void> {
    return request<void>(`/api/sessions/${encodeURIComponent(id)}`, { method: 'DELETE' })
}
```

**Step 2: Add removeSession state helper**

In `web/src/lib/state.svelte.ts`:

```typescript
export function removeSession(sessionId: string): void {
    const nextByDate = new Map(appState.sessionsByDate)
    for (const [date, sessions] of nextByDate) {
        const filtered = sessions.filter((s) => s.id !== sessionId)
        if (filtered.length !== sessions.length) {
            nextByDate.set(date, filtered)
        }
    }
    appState.sessionsByDate = nextByDate

    if (appState.sessionDetails.has(sessionId)) {
        const nextDetails = new Map(appState.sessionDetails)
        nextDetails.delete(sessionId)
        appState.sessionDetails = nextDetails
    }
}
```

**Step 3: Add delete button to SessionCard**

In `web/src/components/SessionCard.svelte`:

Add `onDelete` prop:
```typescript
onDelete: (id: string) => Promise<void>
```

Add delete button in the expanded details section, after the summary section:

```svelte
{#if expanded && detail}
    <div class="session-actions">
        <button
            type="button"
            class="delete-btn"
            onclick={async (e) => {
                e.stopPropagation()
                if (confirm('Delete this session? This cannot be undone.')) {
                    await onDelete(session.id)
                }
            }}
        >
            Delete session
        </button>
    </div>
{/if}
```

Add styles:
```css
.session-actions {
    border-top: 1px solid var(--line);
    padding: 0.5rem 1rem;
    display: flex;
    justify-content: flex-end;
}

.delete-btn {
    font-size: 0.75rem;
    padding: 0.25rem 0.5rem;
    border: 1px solid var(--danger);
    border-radius: 4px;
    background: transparent;
    color: var(--danger);
    cursor: pointer;
}

.delete-btn:hover {
    background: var(--danger);
    color: #fff;
}
```

**Step 4: Add filtering to SessionList**

In `web/src/components/SessionList.svelte`:

Add a filter function and show/hide toggle:

```typescript
let showHidden = $state<Record<string, boolean>>({})

function isShortSession(session: SessionSummary): boolean {
    if (!session.ended_at) return false
    const durationMs = Date.parse(session.ended_at) - Date.parse(session.started_at)
    const durationMins = durationMs / 60000
    if (durationMins >= 2) return false
    // Short session — hide if summary is empty/failed/boilerplate
    if (!session.summary || session.summary_status !== 'completed') return true
    // Strip markdown headers and check remaining content length
    const content = session.summary.replace(/^#{1,6}\s+.*$/gm, '').trim()
    return content.length < 50
}
```

Update the rendering to filter and show hidden count:

```svelte
{@const allSessions = sessionsByDate.get(date) ?? []}
{@const hiddenCount = showHidden[date] ? 0 : allSessions.filter(isShortSession).length}
{@const visibleSessions = showHidden[date] ? allSessions : allSessions.filter((s) => !isShortSession(s))}

{#if visibleSessions.length > 0}
    <div class="card-stack">
        {#each visibleSessions as session (session.id)}
            <SessionCard ... />
        {/each}
    </div>
{/if}

{#if hiddenCount > 0}
    <button type="button" class="show-hidden" onclick={() => (showHidden[date] = true)}>
        {hiddenCount} short session{hiddenCount > 1 ? 's' : ''} hidden
    </button>
{/if}
```

**Step 5: Wire up onDelete in App.svelte and SessionList**

Pass `onDelete` through SessionList → SessionCard. In `App.svelte`:

```typescript
import { deleteSession } from './lib/api'
import { removeSession } from './lib/state.svelte'

async function handleDeleteSession(id: string): Promise<void> {
    await deleteSession(id)
    removeSession(id)
}
```

Pass to `<SessionList onDelete={handleDeleteSession} .../>` and forward through to SessionCard.

**Step 6: Run frontend tests**

Run: `cd web && npm test`
Expected: PASS (update any tests that assert on SessionCard props).

**Step 7: Commit**

```
jj describe -m "feat: add session delete and filter short/empty sessions"
jj new
```

---

## Feature 4: User-Initiated Session Merge

### Task 4.1: Add merged_into column and merge storage method (backend)

**Files:**
- Modify: `internal/storage/sqlite.go` (migration, MergeSessions method, filter merged)
- Modify: `internal/storage/sqlite_test.go` (add tests)

**Step 1: Write failing tests**

Test `MergeSessions`:
- Creates 2 sessions with segments, merges them, verifies new session exists with all segments
- Verifies source sessions have `status = 'merged'` and `merged_into = newID`
- Verifies `GetSessionsByDate` excludes merged sessions

**Step 2: Add migration**

In `init()`, add:

```go
`ALTER TABLE sessions ADD COLUMN merged_into TEXT NOT NULL DEFAULT ''`,
```

**Step 3: Implement MergeSessions**

```go
func (s *SQLiteStore) MergeSessions(newID string, sourceIDs []string, startedAt, endedAt time.Time) error {
    tx, err := s.db.Begin()
    if err != nil {
        return fmt.Errorf("begin merge tx: %w", err)
    }
    defer func() { _ = tx.Rollback() }()

    // Create the merged session
    _, err = tx.Exec(
        `INSERT INTO sessions(id, started_at, ended_at, status, summary_status) VALUES(?, ?, ?, 'ended', 'pending')`,
        newID,
        startedAt.UTC().Format(time.RFC3339Nano),
        endedAt.UTC().Format(time.RFC3339Nano),
    )
    if err != nil {
        return fmt.Errorf("create merged session: %w", err)
    }

    // Move segments to merged session
    placeholders := make([]string, len(sourceIDs))
    args := make([]any, 0, len(sourceIDs)+1)
    args = append(args, newID)
    for i, id := range sourceIDs {
        placeholders[i] = "?"
        args = append(args, id)
    }
    query := fmt.Sprintf(
        `UPDATE segments SET session_id = ? WHERE session_id IN (%s)`,
        strings.Join(placeholders, ","),
    )
    if _, err := tx.Exec(query, args...); err != nil {
        return fmt.Errorf("move segments: %w", err)
    }

    // Collect audio paths from source sessions
    audioQuery := fmt.Sprintf(
        `SELECT audio_path FROM sessions WHERE id IN (%s) AND audio_path != '' ORDER BY started_at ASC`,
        strings.Join(placeholders, ","),
    )
    rows, err := tx.Query(audioQuery, args[1:]...)
    if err != nil {
        return fmt.Errorf("query audio paths: %w", err)
    }
    var audioPaths []string
    for rows.Next() {
        var p string
        if err := rows.Scan(&p); err != nil {
            _ = rows.Close()
            return fmt.Errorf("scan audio path: %w", err)
        }
        audioPaths = append(audioPaths, p)
    }
    _ = rows.Close()

    if len(audioPaths) > 0 {
        _, err = tx.Exec(`UPDATE sessions SET audio_path = ? WHERE id = ?`,
            strings.Join(audioPaths, ","), newID)
        if err != nil {
            return fmt.Errorf("set merged audio paths: %w", err)
        }
    }

    // Mark source sessions as merged
    markQuery := fmt.Sprintf(
        `UPDATE sessions SET status = 'merged', merged_into = ? WHERE id IN (%s)`,
        strings.Join(placeholders, ","),
    )
    markArgs := make([]any, 0, len(sourceIDs)+1)
    markArgs = append(markArgs, newID)
    markArgs = append(markArgs, args[1:]...)
    if _, err := tx.Exec(markQuery, markArgs...); err != nil {
        return fmt.Errorf("mark sources merged: %w", err)
    }

    return tx.Commit()
}
```

**Step 4: Update GetSessionsByDate to exclude merged**

Change the filter from:
```sql
AND status != 'discarded'
```
to:
```sql
AND status NOT IN ('discarded', 'merged')
```

**Step 5: Run tests**

Run: `go test ./internal/storage/ -v`
Expected: PASS

**Step 6: Commit**

```
jj describe -m "feat: add session merge storage layer with merged_into tracking"
jj new
```

---

### Task 4.2: Add merge sessions API endpoint (backend)

**Files:**
- Modify: `internal/server/api.go` (new POST route)
- Modify: `internal/server/server.go` (ControlHooks)
- Modify: `internal/server/api_test.go` (add tests)

**Step 1: Write failing tests**

Test `POST /api/sessions/merge`:
- Returns 200 with new session data on success
- Returns 400 if fewer than 2 session IDs
- Returns 400 if sessions don't all exist

**Step 2: Extend SessionStore interface**

Add to `SessionStore` in `api.go`:

```go
MergeSessions(newID string, sourceIDs []string, startedAt, endedAt time.Time) error
```

**Step 3: Add ControlHooks for merge**

In `server.go`, add to ControlHooks:

```go
OnSessionMerged func(ctx context.Context, sessionID string) // Triggers summary generation
```

**Step 4: Implement the endpoint**

```go
mux.HandleFunc("POST /api/sessions/merge", func(w http.ResponseWriter, r *http.Request) {
    r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
    var body struct {
        SessionIDs []string `json:"session_ids"`
    }
    if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
        writeJSONError(w, http.StatusBadRequest, "invalid request body")
        return
    }
    if len(body.SessionIDs) < 2 {
        writeJSONError(w, http.StatusBadRequest, "at least 2 session IDs required")
        return
    }

    // Validate all sessions exist and collect time bounds
    var earliest time.Time
    var latest time.Time
    for _, id := range body.SessionIDs {
        if !validSessionID(id) {
            writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("invalid session id: %s", id))
            return
        }
        sess, err := store.GetSession(id)
        if err != nil {
            writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("session not found: %s", id))
            return
        }
        if earliest.IsZero() || sess.StartedAt.Before(earliest) {
            earliest = sess.StartedAt
        }
        if sess.EndedAt != nil && (latest.IsZero() || sess.EndedAt.After(latest)) {
            latest = *sess.EndedAt
        }
    }

    newID := earliest.UTC().Format("20060102150405") + "-merged"
    if err := store.MergeSessions(newID, body.SessionIDs, earliest, latest); err != nil {
        writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("merge sessions: %v", err))
        return
    }

    // Trigger summary generation for the merged session
    if controls.OnSessionMerged != nil {
        go controls.OnSessionMerged(context.Background(), newID)
    }

    merged, err := store.GetSession(newID)
    if err != nil {
        writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("get merged session: %v", err))
        return
    }

    writeJSON(w, http.StatusOK, merged)
})
```

**Step 5: Wire up OnSessionMerged in main.go**

Connect to session Manager's `generateSummary` method (needs a new exported wrapper).

**Step 6: Run tests**

Run: `go test ./internal/server/ -v`
Expected: PASS

Run: `go test ./...`
Expected: All PASS

**Step 7: Commit**

```
jj describe -m "feat: add POST /api/sessions/merge endpoint"
jj new
```

---

### Task 4.3: Add merge UI (frontend)

**Files:**
- Modify: `web/src/lib/api.ts` (add mergeSessions)
- Modify: `web/src/lib/types.ts` (if needed)
- Modify: `web/src/components/SessionList.svelte` (multi-select + merge button)
- Modify: `web/src/components/SessionCard.svelte` (checkbox in select mode)
- Modify: `web/src/App.svelte` (merge handler)
- Modify: `web/src/app.css` (select mode styles)

**Step 1: Add mergeSessions API function**

In `web/src/lib/api.ts`:

```typescript
export function mergeSessions(sessionIds: string[]): Promise<SessionSummary> {
    return request<SessionSummary>('/api/sessions/merge', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ session_ids: sessionIds }),
    })
}
```

**Step 2: Add multi-select state to SessionList**

In `web/src/components/SessionList.svelte`:

```typescript
let selectMode = $state(false)
let selectedIds = $state<Set<string>>(new Set())

function toggleSelect(id: string) {
    const next = new Set(selectedIds)
    if (next.has(id)) {
        next.delete(id)
    } else {
        next.add(id)
    }
    selectedIds = next
}

function exitSelectMode() {
    selectMode = false
    selectedIds = new Set()
}
```

Add props:
```typescript
onMerge: (sessionIds: string[]) => Promise<void>
```

Add UI: "Select" button in panel header, "Merge (N)" button when 2+ selected, "Cancel" button to exit.

**Step 3: Add checkbox to SessionCard in select mode**

Pass `selectMode` and `selected` props to SessionCard. When `selectMode` is true, show a checkbox instead of the normal click behavior.

```svelte
{#if selectMode}
    <input type="checkbox" checked={selected} onchange={() => onToggleSelect(session.id)} />
{/if}
```

**Step 4: Wire merge handler in App.svelte**

```typescript
import { mergeSessions } from './lib/api'

async function handleMerge(sessionIds: string[]): Promise<void> {
    await mergeSessions(sessionIds)
    // Refresh sessions for affected dates
    const affectedDates = new Set<string>()
    for (const [date, sessions] of appState.sessionsByDate) {
        if (sessions.some((s) => sessionIds.includes(s.id))) {
            affectedDates.add(date)
        }
    }
    for (const date of affectedDates) {
        const sessions = await fetchSessions(date)
        setSessionsForDate(date, sessions)
    }
}
```

**Step 5: Add select mode styles**

```css
.select-mode-bar {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    padding: 0.5rem 0;
}

.merge-btn {
    /* reuse accent button styles */
}

.session-checkbox {
    margin-right: 0.5rem;
}
```

**Step 6: Handle multi-audio playback in AudioPlayer**

In `web/src/components/AudioPlayer.svelte`, detect comma-separated `audio_path` and play files in sequence. This is a stretch goal — if audio paths contain commas, split and create multiple audio elements. Can be a follow-up if complex.

**Step 7: Run frontend tests**

Run: `cd web && npm test`
Expected: PASS

**Step 8: Commit**

```
jj describe -m "feat: add session multi-select and merge UI"
jj new
```

---

## Final Verification

After all features are implemented:

1. Run full backend tests: `go test ./...`
2. Run full frontend tests: `cd web && npm test`
3. Manual smoke test with dev server:
   - Start a session, reload page → live transcript restored
   - Check collapsed session cards → markdown rendered properly
   - Verify short sessions are hidden with "N hidden" toggle
   - Delete a junk session → confirm it's gone
   - Select 2 sessions → merge → verify combined session appears
