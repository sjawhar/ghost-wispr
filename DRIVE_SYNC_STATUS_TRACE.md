# Drive Sync Component Status: Complete Code Path Trace

## Executive Summary

The "Drive Sync:error" status shown in the UI is produced by **sync orchestrator callbacks in the session manager** when Google Drive sync fails. The status flows through the websocket event system and is consumed by the frontend state management.

---

## Backend: Status Production

### 1. **Source: Session Manager** (`internal/session/manager.go`)

The sync status is set in **two locations** within the session end flow:

#### Location A: After Quick Refinement (lines 383-396)
```go
if syncer != nil {
    go func() {
        if err := syncer.SyncSession(context.Background(), sessionID); err != nil {
            m.logger.Error("gdrive sync failed", "operation", "sync_session", "session_id", sessionID, "error", err)
            if m.hub != nil {
                m.hub.BroadcastComponentStatus("sync", storage.ComponentStatusError, fmt.Sprintf("Google Drive sync failed for session %s", sessionID))
            }
            return
        }
        if m.hub != nil {
            m.hub.BroadcastComponentStatus("sync", storage.ComponentStatusConnected, fmt.Sprintf("Google Drive sync completed for session %s", sessionID))
        }
    }()
}
```

#### Location B: After Full Summarization (lines 465-478)
```go
if syncer != nil {
    go func() {
        if err := syncer.SyncSession(context.Background(), sessionID); err != nil {
            m.logger.Error("gdrive sync failed", "operation", "sync_session", "session_id", sessionID, "error", err)
            if m.hub != nil {
                m.hub.BroadcastComponentStatus("sync", storage.ComponentStatusError, fmt.Sprintf("Google Drive sync failed for session %s", sessionID))
            }
            return
        }
        if m.hub != nil {
            m.hub.BroadcastComponentStatus("sync", storage.ComponentStatusConnected, fmt.Sprintf("Google Drive sync completed for session %s", sessionID))
        }
    }()
}
```

**Key Points:**
- Status is set **asynchronously** via goroutine after session ends
- Error status is set when `syncer.SyncSession()` returns an error
- Success status is set when sync completes without error
- The `syncer` is injected into the manager (likely a Google Drive sync orchestrator)

### 2. **Event Broadcasting: Hub** (`internal/server/hub.go`)

The `BroadcastComponentStatus` method (lines 141-154):

```go
func (h *Hub) BroadcastComponentStatus(component, status, message string) {
    event := ComponentStatusEvent{
        Event:     newEvent("component_status", time.Now().UTC()),
        Component: component,
        Status:    status,
        Message:   message,
    }

    h.mu.Lock()
    h.componentStatuses[component] = event  // Store latest status
    h.mu.Unlock()

    h.broadcastEvent(event)  // Send to all connected clients
}
```

**Key Points:**
- Stores the **latest status** in `componentStatuses` map (keyed by component name)
- Broadcasts to all connected websocket clients
- Event is persisted to event store if configured

### 3. **Event Type Definition** (`internal/server/events.go`)

```go
type ComponentStatusEvent struct {
    Event
    Component string `json:"component"`
    Status    string `json:"status"`
    Message   string `json:"message"`
}
```

### 4. **Status Constants** (`internal/status/status.go`)

```go
const (
    ComponentStatusConnected    = "connected"
    ComponentStatusDisconnected = "disconnected"
    ComponentStatusReconnecting = "reconnecting"
    ComponentStatusError        = "error"
    ComponentStatusOK           = "ok"
    ComponentStatusOpen         = "open"
    ComponentStatusClosed       = "closed"
)
```

---

## WebSocket Transport

### 1. **Connection Handler** (`internal/server/ws.go`, lines 18-57)

When a client connects:

```go
// 1. Send connection event
connectionEvent := ConnectionEvent{
    Event:     newEvent("connection", time.Now().UTC()),
    Connected: true,
}
payload, err := json.Marshal(connectionEvent)
if err == nil {
    _ = conn.WriteMessage(websocket.TextMessage, payload)
}

// 2. Subscribe to future events
ch := hub.Subscribe()
defer hub.Unsubscribe(ch)

// 3. Send snapshot of current component statuses
for _, statusEvent := range hub.SnapshotComponentStatuses() {
    payload, err := json.Marshal(statusEvent)
    if err != nil {
        continue
    }
    if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
        return
    }
}

// 4. Stream future events
for msg := range ch {
    if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
        return
    }
}
```

**Key Points:**
- **Snapshot sent on connection** - all current component statuses are sent immediately
- **Future events streamed** - new status changes are sent as they occur
- This is why the UI shows the error even if it happened before the page loaded

---

## Frontend: Status Consumption

### 1. **State Management** (`web/src/lib/state.svelte.ts`)

#### State Definition (lines 12-16, 32)
```typescript
export type ComponentStatus = {
  status: ComponentStatusValue
  message: string
  timestamp: string
}

type AppState = {
  // ...
  componentStatuses: Record<string, ComponentStatus>
}
```

#### Event Handler (lines 176-178)
```typescript
case 'component_status':
  applyComponentStatus(event)
  return
```

#### Status Application (lines 184-193)
```typescript
export function applyComponentStatus(event: ComponentStatusEvent): void {
  appState.componentStatuses = {
    ...appState.componentStatuses,
    [event.component]: {
      status: event.status,
      message: event.message,
      timestamp: event.timestamp,
    },
  }
}
```

### 2. **UI Rendering** (`web/src/components/SystemStatus.svelte`)

#### Status Retrieval (line 6)
```typescript
const syncStatus = $derived(appState.componentStatuses['sync'])
```

#### Status Display (lines 96-101)
```svelte
<div class="status-item" data-testid="status-sync">
  <span class="status-dot" style="background-color: {getStatusColor(syncStatus?.status)}"></span>
  <span class="status-label" style="color: {getStatusColor(syncStatus?.status)}"
    >Drive Sync: {syncStatus?.status || 'unknown'}</span
  >
</div>
```

#### Color Mapping (lines 10-30)
```typescript
function getStatusColor(status?: ComponentStatus) {
  switch (status) {
    case ComponentStatus.Connected:
    case ComponentStatus.Synced:
    case ComponentStatus.Open:
    case ComponentStatus.Ok:
      return 'var(--success)'
    case ComponentStatus.Unavailable:
    case ComponentStatus.Disabled:
    case ComponentStatus.Closed:
      return 'var(--warning)'
    case ComponentStatus.Disconnected:
    case ComponentStatus.Error:
      return 'var(--danger)'
    // ...
  }
}
```

**Key Points:**
- `ComponentStatus.Error` maps to `var(--danger)` (red color)
- Status is derived reactively from `appState.componentStatuses['sync']`
- Message is stored but not displayed in the status header

---

## Complete Data Flow

```
┌─────────────────────────────────────────────────────────────────┐
│ BACKEND: Session End Flow                                       │
├─────────────────────────────────────────────────────────────────┤
│                                                                 │
│  1. Session ends (quick refinement or full summarization)      │
│  2. syncer.SyncSession(sessionID) called asynchronously        │
│  3. If error:                                                   │
│     → hub.BroadcastComponentStatus(                            │
│         "sync",                                                 │
│         storage.ComponentStatusError,  // = "error"            │
│         "Google Drive sync failed for session {id}"            │
│       )                                                         │
│  4. If success:                                                 │
│     → hub.BroadcastComponentStatus(                            │
│         "sync",                                                 │
│         storage.ComponentStatusConnected,  // = "connected"    │
│         "Google Drive sync completed for session {id}"         │
│       )                                                         │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘
                              ↓
┌─────────────────────────────────────────────────────────────────┐
│ HUB: Event Storage & Broadcasting                               │
├─────────────────────────────────────────────────────────────────┤
│                                                                 │
│  1. Store in componentStatuses["sync"] = event                 │
│  2. Broadcast to all connected websocket clients               │
│  3. Persist to event store (if configured)                     │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘
                              ↓
┌─────────────────────────────────────────────────────────────────┐
│ WEBSOCKET: Transport                                            │
├─────────────────────────────────────────────────────────────────┤
│                                                                 │
│  On new connection:                                             │
│  1. Send connection event                                       │
│  2. Send snapshot of all current component statuses             │
│     (includes latest sync status)                               │
│  3. Stream future status changes                                │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘
                              ↓
┌─────────────────────────────────────────────────────────────────┐
│ FRONTEND: State Management                                      │
├─────────────────────────────────────────────────────────────────┤
│                                                                 │
│  1. Receive component_status event                              │
│  2. applyComponentStatus() updates appState:                    │
│     appState.componentStatuses['sync'] = {                      │
│       status: "error",                                          │
│       message: "Google Drive sync failed for session {id}",     │
│       timestamp: "2026-04-11T..."                               │
│     }                                                           │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘
                              ↓
┌─────────────────────────────────────────────────────────────────┐
│ FRONTEND: UI Rendering                                          │
├─────────────────────────────────────────────────────────────────┤
│                                                                 │
│  1. syncStatus = appState.componentStatuses['sync']             │
│  2. Display: "Drive Sync: error"                                │
│  3. Color: getStatusColor("error") → var(--danger) [RED]        │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘
```

---

## Key Findings

### Source of "Drive Sync:error"

**Primary Source:** `syncer.SyncSession()` error in `internal/session/manager.go`

- **Trigger:** When a session ends (either quick refinement or full summarization)
- **Condition:** The Google Drive sync orchestrator returns an error
- **Execution:** Asynchronous goroutine (non-blocking)
- **Status Value:** `storage.ComponentStatusError` = `"error"`

### Why It Persists

1. **Status is stored** in `hub.componentStatuses["sync"]`
2. **Snapshot sent on connection** - any new client receives the last known status
3. **No automatic recovery** - status remains until explicitly changed by another sync attempt

### Related Status Sources

Other components that set status via `BroadcastComponentStatus`:

| Component | Source | Trigger |
|-----------|--------|---------|
| `mic` | `cmd/ghost-wispr/main.go` | Microphone connection/disconnection events |
| `deepgram` | `cmd/ghost-wispr/main.go` | Deepgram resilient client state changes |
| `summary` | `internal/session/manager.go` | Summarization failures |
| `refinement` | `internal/session/manager.go` | Batch refinement failures |

### No Health Check Dependency

**Important:** The sync status is **NOT** based on:
- ❌ Health check endpoints (`/healthz/live`, `/healthz/ready`)
- ❌ Periodic polling
- ❌ Component availability checks

It is **purely event-driven** from the sync orchestrator callback.

---

## Files Involved

### Backend
- `internal/session/manager.go` - **Sync status source** (lines 383-396, 465-478)
- `internal/server/hub.go` - **Event broadcasting** (lines 141-154)
- `internal/server/ws.go` - **WebSocket transport** (lines 41-49)
- `internal/server/events.go` - **Event type definition**
- `internal/status/status.go` - **Status constants**

### Frontend
- `web/src/lib/state.svelte.ts` - **State management** (lines 176-193)
- `web/src/components/SystemStatus.svelte` - **UI rendering** (lines 6, 96-101)
- `web/src/lib/types.ts` - **Type definitions**

---

## Verification Points

To verify this trace:

1. **Check sync error source:**
   ```bash
   grep -n "syncer.SyncSession" internal/session/manager.go
   ```

2. **Check broadcast calls:**
   ```bash
   grep -n 'BroadcastComponentStatus.*"sync"' internal/session/manager.go
   ```

3. **Check websocket snapshot:**
   ```bash
   grep -n "SnapshotComponentStatuses" internal/server/ws.go
   ```

4. **Check frontend state update:**
   ```bash
   grep -n "applyComponentStatus" web/src/lib/state.svelte.ts
   ```

5. **Check UI rendering:**
   ```bash
   grep -n "syncStatus" web/src/components/SystemStatus.svelte
   ```
