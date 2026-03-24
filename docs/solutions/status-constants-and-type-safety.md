---
title: "Status Constants and Type Safety: No Magic Strings"
category: general
tags:
  - type-safety
  - enums
  - constants
  - status
  - frontend
  - backend
date: 2026-03-24
status: active
symptoms:
  - "status color missing"
  - "switch statement doesn't handle new status"
  - "synced showing gray instead of green"
  - "unknown status value"
---

# Status Constants and Type Safety: No Magic Strings

## The Problem

Ghost Whisper has many status values: session status, summary status, sync status, sync state, refinement status, component health, transcript source. These were originally scattered as raw strings across 32 files (133 in Go, 53 in TypeScript).

This caused a real bug: the `getStatusColor()` function in `SystemStatus.svelte` handled `connected` (green) and `disconnected` (red) but not `synced`. Drive Sync showed gray instead of green because `synced` fell through to the default case.

## The Solution

### Backend: `internal/status/status.go`

Single source of truth for all status values:

```go
package status

// Session
const SessionActive = "active"
const SessionEnded = "ended"
const SessionDiscarded = "discarded"

// Summary
const SummaryPending = "pending"
const SummaryRunning = "running"
const SummaryCompleted = "completed"
const SummaryFailed = "failed"

// Sync state
const SyncStatePending = "PENDING"
const SyncStateSynced = "SYNCED"
// ... etc

// Component health
const ComponentConnected = "connected"
const ComponentDisconnected = "disconnected"
// ... etc
```

Other packages import from `status` and alias locally if needed for backward compatibility:
```go
const SummaryPending = gwstatus.SummaryPending
```

### Frontend: `web/src/lib/types.ts`

TypeScript enums mirror the Go constants:

```typescript
export enum ComponentStatus {
  Connected = 'connected',
  Disconnected = 'disconnected',
  Reconnecting = 'reconnecting',
  Error = 'error',
  Open = 'open',
  Closed = 'closed',
  Synced = 'synced',
  Ok = 'ok',
}
```

Components use enum values in switch statements. TypeScript warns about unhandled enum cases, so the `synced`-not-green bug becomes a compile-time error.

## Rules

1. **Adding a new status value**: Update `internal/status/status.go` AND `web/src/lib/types.ts`. Then update every switch statement that handles that status family.

2. **String values must not change**: The DB stores these strings, the API returns them, the WebSocket sends them. Only change how they're REFERENCED in code, not the values themselves.

3. **Test files can use raw strings**: For readability in tests, raw strings are acceptable. Application code must use constants/enums.

4. **Color mapping must be exhaustive**: If you add a new `ComponentStatus` enum value, `getStatusColor()` in `SystemStatus.svelte` must handle it. Same for `getHealthColor()`.
