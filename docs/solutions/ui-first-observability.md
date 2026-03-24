---
title: "UI-First Observability: The Web UI Is the Primary Diagnostic Surface"
category: general
tags:
  - design-philosophy
  - observability
  - ui
  - logging
  - error-handling
date: 2026-03-24
status: active
symptoms:
  - "user can't tell what's wrong"
  - "errors only visible in logs"
  - "blank page on startup"
  - "status shows unknown"
---

# UI-First Observability: The Web UI Is the Primary Diagnostic Surface

## The Principle

**Nobody looks at the logs.** The web UI is the single surface for humans to understand what's happening and fix problems. Logs exist for agents and historical reference — not as the primary diagnostic tool.

This means:
- Every error must be visible in the UI, not just logged to stdout
- The app must never show a blank page — if config is missing, start anyway and show what's degraded
- Component status (Deepgram, mic, Drive sync) must be visible at a glance with color coding
- Users must be able to retry failed operations from the UI (re-summarize, re-sync, re-refine)

## What This Looks Like in Practice

### Status Header
`SystemStatus.svelte` shows all component statuses with color coding:
- Green: connected, synced, open, ok
- Yellow: reconnecting, draining, pending
- Red: disconnected, error, closed, failed
- Gray: unknown/loading

On mount, it fetches from `/healthz/ready` to populate initial state (don't wait for WebSocket events).

### Error Propagation
Backend errors are broadcast via WebSocket `component_status` events. The hub has `BroadcastComponentStatus(component, status, message)` which pushes to all connected clients. The UI shows these in the status header AND on individual session cards.

### Log Viewer
`LogViewer.svelte` reads from `GET /api/logs?level=error&limit=100`. Logs are captured in-memory by `logging.NewLogSink(1000)` which tees slog output to both stdout and a ring buffer. The UI can filter by level and auto-scrolls.

### Retry Controls
Session cards show retry buttons for failed summaries, syncs, and batch refinements. Each calls a POST endpoint that re-triggers the operation.

### Graceful Degradation
`internal/config/startup.go` validates all components at startup. Missing Deepgram key? App starts, serves UI, shows "transcription unavailable." Missing mic? Same — API and UI still work. The user always sees what's available and what's not.

## Gotchas

1. **Initial state on page load**: The SystemStatus component must fetch from `/healthz/ready` on mount. If it only listens for WebSocket events, everything shows "unknown" on first load because no events have been received yet. The overall health derivation treated all-undefined as "healthy" (because `!undefined` is true in JS).

2. **CSS variables must be defined**: If you use `var(--success)` for green status text, the variable must actually exist in `app.css`. We had a bug where `--success` and `--warning` were used in 6+ components but never defined — only `--danger` existed. Colors silently fell back to transparent.

3. **Don't crash, degrade**: "Fail fast" is wrong for this app. If the Deepgram API key is missing, the app should start and show the error in the UI. A blank page is worse than a degraded page.

4. **Log viewer is in-memory**: The `LogSink` ring buffer is lost on restart. For long-running debugging, check systemd journal. The UI log viewer is for live diagnostics, not historical analysis.
