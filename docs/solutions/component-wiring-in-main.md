---
title: "Component Wiring in main.go: Initialization Order and Dependency Injection"
category: general
tags:
  - architecture
  - dependency-injection
  - main
  - initialization
  - health-checks
date: 2026-03-24
status: active
module: cmd
symptoms:
  - "health check shows all unhealthy"
  - "component is nil at runtime"
  - "feature not working despite code existing"
  - "nil pointer in handler"
---

# Component Wiring in main.go: Initialization Order and Dependency Injection

## The Pattern

Ghost Whisper uses dependency injection through `main.go`. Components are created in a specific order and passed to the server handler. If a component is optional (mic, Deepgram, Drive sync), it may be nil — and all consumers must handle nil gracefully.

## The Gotcha That Bit Us

Task 2 created `DefaultHealthChecker` with methods like `IsDeepgramConnected()`. Task 1/3/etc added the health routes to the server. But nobody wired the actual component references into `main.go`. The `DefaultHealthChecker` was created with nil fields, so every health check returned "unhealthy."

The fix was adding this to `main.go` AFTER all components are initialized:

```go
healthChecker := server.NewDefaultHealthChecker(deepgramHealth, store, micStatus, captureReadiness{capture: capture, mic: micStatus}, llmTracker)
handler, err := server.HandlerWithLogger(assets, hub, store, controlHooks, authToken, healthChecker, appLogger, embeddingClient, cfgStore)
```

## Initialization Order

The order in `main.go` matters:

1. **Config** (`cfgStore`) — loaded first, everything depends on it
2. **Logger and LogSink** (`appLogger`, `logSink`, from `startAppLogger`) — second, everything logs. `startAppLogger` is the logger's one constructor: it writes to stdout and to the sink the UI log viewer reads (`GET /api/logs`)
3. **SQLite store** (`store`) — third, most things need the DB
4. **Hub** (`hub`) — WebSocket broadcast, needed by error propagation
5. **Session manager** (`manager`) — depends on store, recorder, summarizer, hub, detector
6. **Batch transcriber** — optional, depends on config; set on the manager
7. **Capture device** (`micDevice`) and **Drive sync orchestrator** — the orchestrator is optional, depends on Drive credentials
8. **Mic and Deepgram client** — started together by the mic start; either may fail (graceful degradation)
9. **Health checker** — depends on the Deepgram client, store, mic status, capture and LLM tracker (must be created AFTER all of these)
10. **HTTP handler** — depends on everything above

If you add a new component, you must:
1. Initialize it in the correct position in this order
2. Pass it to anything that needs it (handler, manager, etc.)
3. Handle the nil case if it's optional
4. Wire it into the health checker if it should be monitored

## Config Hot-Reload

The `cfgStore.OnChange()` callback updates components when config is reloaded:
- Silence timeout, summarizer, batch transcriber, Drive syncer
- The log level is set on the one logger every component holds; nothing builds a new logger, so `GET /api/logs` keeps every line
- New components are created atomically

**Gotcha**: If config reload fails partway through, the system is partially updated. There's no rollback mechanism. This is a known limitation.

## Testing

The health checker uses interfaces (`IsConnected() bool`, `Ping(ctx) error`, `IsOpen() bool`), so tests can inject mocks. The handler constructor accepts the `HealthChecker` interface, not the concrete type.

When writing tests for new endpoints, check that you're passing all required dependencies to the handler constructor. A nil dependency that's not handled will panic at runtime, not at compile time.
