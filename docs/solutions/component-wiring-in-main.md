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
healthChecker := server.NewDefaultHealthChecker(resilientClient, store, mic)
handler, err := server.HandlerWithLogger(assets, hub, store, controlHooks, authToken, healthChecker, appLogger, cfgStore)
```

## Initialization Order

The order in `main.go` matters:

1. **Config** (`cfgStore`) — loaded first, everything depends on it
2. **Logger** (`appLogger`) — second, everything logs
3. **SQLite store** (`store`) — third, most things need the DB
4. **LogSink** (`logSink`) — captures logs for the UI log viewer
5. **Hub** (`hub`) — WebSocket broadcast, needed by error propagation
6. **PortAudio + Mic** (`mic`) — may fail (graceful degradation)
7. **Deepgram client** (`resilientClient`) — may fail (graceful degradation)
8. **Batch transcriber** — optional, depends on config
9. **Session manager** (`manager`) — depends on store, summarizer, batch transcriber
10. **Sync orchestrator** — optional, depends on Drive credentials
11. **Health checker** — depends on resilientClient, store, mic (must be created AFTER all of these)
12. **HTTP handler** — depends on everything above

If you add a new component, you must:
1. Initialize it in the correct position in this order
2. Pass it to anything that needs it (handler, manager, etc.)
3. Handle the nil case if it's optional
4. Wire it into the health checker if it should be monitored

## Config Hot-Reload

The `cfgStore.OnChange()` callback updates components when config is reloaded:
- Silence timeout, summarizer, batch transcriber, Drive syncer
- Logger level is updated
- New components are created atomically

**Gotcha**: If config reload fails partway through, the system is partially updated. There's no rollback mechanism. This is a known limitation.

## Testing

The health checker uses interfaces (`IsConnected() bool`, `Ping(ctx) error`, `IsOpen() bool`), so tests can inject mocks. The handler constructor accepts the `HealthChecker` interface, not the concrete type.

When writing tests for new endpoints, check that you're passing all required dependencies to the handler constructor. A nil dependency that's not handled will panic at runtime, not at compile time.
