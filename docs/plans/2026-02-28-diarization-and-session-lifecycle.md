# Diarization Quality & Session Lifecycle

**Date:** 2026-02-28
**Issues:** #3 (Improve speaker diarization quality), #4 (Manual session end button)
**Status:** Approved

## Problem

### Issue #3: Diarization Fragmentation

The current transcription pipeline treats every Deepgram `is_final` message as a standalone unit. Each message triggers `GroupWordsBySpeaker()` which creates and immediately persists segments. This causes three problems:

1. **Fragmented sentences:** A speaker pausing mid-sentence causes Deepgram to send multiple `is_final` messages, creating separate segments for what should be one continuous turn. In a real 3.5-hour session: 988 raw segments, 528 (53%) of which were consecutive same-speaker within 3 seconds.

2. **Misattributed speakers:** Short segments (1-2 words) frequently get assigned wrong speaker IDs. 212 segments had 5 characters or fewer, and 79 showed A→B→A flip-flop patterns. Example: "I I" (speaker 0) → "think" (speaker 1) → "if you give me admin access..." (speaker 0).

3. **Aggressive endpointing:** Deepgram's default endpointing is ~10ms, meaning it finalizes after the tiniest pause. The current config doesn't set `endpointing`, `interim_results`, `utterance_end_ms`, or `vad_events`.

**Root cause:** The code doesn't use Deepgram's streaming API correctly. The intended pattern is to buffer `is_final` chunks and flush on `speech_final`, not to treat each `is_final` as an independent utterance.

### Issue #4: No Manual Session End

Sessions are created implicitly (on first speech) and ended implicitly (30s silence timeout). The `ForceEndSession()` method exists on the manager but isn't exposed via any API endpoint. There is no way for a user to explicitly end a session.

## Research: Industry-Standard Deepgram Parameters

Real-world production configurations from open-source projects:

| Project | Use Case | endpointing | utterance_end_ms | interim_results | vad_events |
|---------|----------|-------------|------------------|-----------------|------------|
| TEN-framework | Voice agents | 300ms | 1000ms | true | - |
| Bolna | Telephony AI | 400ms | - | true | - |
| Gabber | Conversational AI | 750ms | 1000ms | true | true |
| Pipecat | Voice pipelines | configurable | 1000ms | - | true |

**Consensus:** `endpointing=300-400`, `utterance_end_ms=1000`, `interim_results=true`. Meeting transcription favors higher endpointing (400ms) for quality over low-latency voice agents (100-300ms).

## Design

### Issue #3: Diarization Quality (Two-Layer Fix)

#### Layer 1: Deepgram Parameter Tuning

New `transcription:` config section (generic naming — provider-agnostic in case we switch from Deepgram):

```yaml
transcription:
  endpointing: 400          # ms of silence before Deepgram finalizes a chunk
  utterance_end_ms: 1000     # ms of silence before UtteranceEnd fires
```

`interim_results` and `vad_events` are always-on (not configurable).

In Go, these map to `LiveTranscriptionOptions`:

```go
tOptions := &interfaces.LiveTranscriptionOptions{
    Model:          "nova-2",
    Language:       "en-US",
    Diarize:        true,
    Punctuate:      true,
    SmartFormat:    true,
    Encoding:       "linear16",
    SampleRate:     selectedSampleRate,
    Channels:       1,
    Endpointing:    cfg.Transcription.Endpointing,    // "400"
    InterimResults: true,
    UtteranceEndMs: cfg.Transcription.UtteranceEndMs,  // "1000"
    VadEvents:      true,
}
```

#### Layer 2: Utterance Buffering

New `UtteranceBuffer` in the session package. Replaces the current "process every is_final immediately" pattern with Deepgram's intended streaming pattern:

**Message flow:**
```
Deepgram message arrives:
  is_final=false  → broadcast interim text to UI (faded)
  is_final=true   → accumulate words in buffer
  speech_final=true → flush buffer: GroupWordsBySpeaker() → persist + broadcast (solid)
  UtteranceEnd    → flush any remaining buffer
```

**Manager changes from:**
```
Message(mr) → GroupWordsBySpeaker(words) → store each segment → broadcast each segment
```

**To:**
```
Message(mr) →
  if interim: broadcast interim event
  if is_final: buffer.AddWords(words)
  if speech_final: segments = buffer.Flush() → GroupWordsBySpeaker → store + broadcast
UtteranceEnd → buffer.Flush() → store + broadcast remaining
```

This ensures `GroupWordsBySpeaker()` runs on complete utterances (all words between pauses) rather than on tiny intermediate chunks, giving Deepgram's diarization the full context it needs.

#### Layer 3: Interim Display (Notion-style)

New WebSocket event `live_transcript_interim`:

```json
{
  "type": "live_transcript_interim",
  "text": "and then I went to the store",
  "speaker": 0,
  "start_time": 123.4
}
```

Frontend behavior:
- On `live_transcript_interim` → show faded text at bottom of live panel, replacing previous interim
- On `live_transcript` → clear interim text, append finalized segment with solid styling
- CSS: interim text gets `opacity: 0.5` and italic styling

This gives the user immediate visual feedback while speech is being processed, with a clear visual distinction between "still being heard" and "committed to transcript."

#### Config additions

```go
type Transcription struct {
    Endpointing    string `yaml:"endpointing"`      // default "400"
    UtteranceEndMs string `yaml:"utterance_end_ms"`  // default "1000"
}
```

### Issue #4: Manual Session End

#### Backend

- **New endpoint:** `POST /api/session/end`
  - Calls `manager.ForceEndSession(ctx)`
  - Returns `204 No Content` on success
  - Returns `409 Conflict` if no active session
  - The system stays listening — a new session starts automatically on next speech
- **ControlHooks:** Add `EndSession func(ctx context.Context) error`

#### Frontend

- **API client:** Add `endSession()` to `api.ts`
- **UI button:** In `Controls.svelte`, "End Session" button visible when `activeSessionId` is non-empty
- **Behavior:** On `session_ended` WebSocket event (already exists), button disappears since `activeSessionId` clears
- **UX:** No confirmation dialog — the action isn't destructive (recording continues)

## What We're NOT Doing

- **Flip-flop speaker correction:** Heuristic that assumed 2 speakers. Dropped because it doesn't generalize to multi-speaker conversations. The correct fix is better buffering so Deepgram has more context.
- **Custom diarization models:** Out of scope. If quality is still insufficient after these changes, the next step is evaluating `nova-3` or alternative providers, not building local speaker models.
- **Pausing recording on session end:** Session end just closes the current session. The system stays active and will start a new session on next speech.

## Expected Impact

Based on the session data analysis (988 segments, 3.5-hour session):
- **Endpointing tuning** should reduce raw segments by ~40-50% (fewer tiny chunks from Deepgram)
- **Utterance buffering** should reduce visible segments by another ~30% (merging within complete utterances)
- **Combined effect:** ~460 merged segments → likely ~250-350 coherent speaker turns
- **Diarization accuracy** should improve because `GroupWordsBySpeaker()` will operate on complete utterances with full context instead of 1-3 word fragments

---

# Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Fix speaker diarization fragmentation by correctly using Deepgram's streaming API (buffering until speech_final), add Notion-style interim display, and add manual session end.

**Architecture:** Tune Deepgram params (endpointing=400, utterance_end_ms=1000, interim_results, vad_events), buffer words in Manager until speech_final, broadcast interim events for faded live text, expose ForceEndSession via HTTP API.

**Tech Stack:** Go 1.24, Deepgram Go SDK v3.5.0, Svelte 5, SQLite

---

### Task 1: Add Transcription config

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`

**Step 1: Add Transcription struct and defaults**

Add to `config.go`:

```go
type Transcription struct {
	Endpointing    string `yaml:"endpointing"`
	UtteranceEndMs string `yaml:"utterance_end_ms"`
}
```

Add `Transcription Transcription` field to `Config` struct.
In `defaults()`, set:
```go
Transcription: Transcription{
	Endpointing:    "400",
	UtteranceEndMs: "1000",
},
```

Add env var overrides in `applyEnvOverrides`:
```go
if v := os.Getenv(EnvPrefix + "TRANSCRIPTION_ENDPOINTING"); v != "" {
	cfg.Transcription.Endpointing = v
}
if v := os.Getenv(EnvPrefix + "TRANSCRIPTION_UTTERANCE_END_MS"); v != "" {
	cfg.Transcription.UtteranceEndMs = v
}
```

**Step 2: Add test for Transcription config defaults**

In `config_test.go`, add test verifying defaults load correctly (`Endpointing="400"`, `UtteranceEndMs="1000"`).

**Step 3: Run tests**

Run: `go test ./internal/config/ -v`
Expected: PASS

**Step 4: Commit**

```
jj describe -m "feat: add transcription config section with endpointing and utterance_end_ms"
```

---

### Task 2: Add interim event types to server

**Files:**
- Modify: `internal/server/events.go`
- Modify: `internal/server/hub.go`
- Modify: `internal/session/types.go`

**Step 1: Add LiveTranscriptInterimEvent to events.go**

```go
type LiveTranscriptInterimEvent struct {
	Event
	Speaker   int     `json:"speaker"`
	Text      string  `json:"text"`
	StartTime float64 `json:"start_time"`
}
```

**Step 2: Add BroadcastLiveTranscriptInterim to hub.go**

```go
func (h *Hub) BroadcastLiveTranscriptInterim(speaker int, text string, startTime float64) {
	h.broadcastEvent(LiveTranscriptInterimEvent{
		Event:     newEvent("live_transcript_interim", time.Now().UTC()),
		Speaker:   speaker,
		Text:      text,
		StartTime: startTime,
	})
}
```

**Step 3: Update EventBroadcaster interface in types.go**

Add `BroadcastLiveTranscriptInterim(speaker int, text string, startTime float64)` to the `EventBroadcaster` interface.

**Step 4: Update hubMock in manager_test.go**

Add the new method to `hubMock` so tests compile.

**Step 5: Run tests**

Run: `go test ./internal/... -v`
Expected: PASS

**Step 6: Commit**

```
jj describe -m "feat: add live_transcript_interim event type and hub broadcast"
```

---

### Task 3: Implement utterance buffer

**Files:**
- Create: `internal/session/buffer.go`
- Create: `internal/session/buffer_test.go`

**Step 1: Write buffer_test.go**

Test cases:
- `TestBuffer_AddWords_AccumulatesWords` — words added to buffer are accumulated
- `TestBuffer_Flush_ReturnsAllWords` — flush returns all accumulated words and empties buffer
- `TestBuffer_Flush_EmptyBuffer` — flush on empty buffer returns nil
- `TestBuffer_AddWords_MultipleCallsAccumulate` — multiple AddWords calls accumulate

**Step 2: Run tests to verify they fail**

Run: `go test ./internal/session/ -run TestBuffer -v`
Expected: FAIL (buffer.go doesn't exist yet)

**Step 3: Implement UtteranceBuffer**

```go
package session

import "github.com/sjawhar/ghost-wispr/internal/transcribe"

type UtteranceBuffer struct {
	words []transcribe.Word
}

func NewUtteranceBuffer() *UtteranceBuffer {
	return &UtteranceBuffer{}
}

func (b *UtteranceBuffer) AddWords(words []transcribe.Word) {
	b.words = append(b.words, words...)
}

func (b *UtteranceBuffer) Flush() []transcribe.Word {
	if len(b.words) == 0 {
		return nil
	}
	out := b.words
	b.words = nil
	return out
}

func (b *UtteranceBuffer) Len() int {
	return len(b.words)
}
```

**Step 4: Run tests**

Run: `go test ./internal/session/ -run TestBuffer -v`
Expected: PASS

**Step 5: Commit**

```
jj describe -m "feat: add UtteranceBuffer for accumulating words until speech_final"
```

---

### Task 4: Refactor Manager to use UtteranceBuffer

**Files:**
- Modify: `internal/session/manager.go`
- Modify: `internal/session/manager_test.go`

**Step 1: Write new test for buffered behavior**

Add `TestManager_BuffersUntilSpeechFinal` — send two `is_final=true` messages (not speech_final), verify no segments persisted. Then send one with `speech_final=true`, verify segments are persisted with merged words.

Add `TestManager_InterimBroadcast` — send `is_final=false` message, verify `BroadcastLiveTranscriptInterim` was called but no segments persisted.

Add `TestManager_UtteranceEndFlushesBuffer` — send `is_final=true` without speech_final, then call `UtteranceEnd()`, verify buffered words are flushed to segments.

**Step 2: Run tests to verify new tests fail**

Run: `go test ./internal/session/ -run TestManager_Buffer -v`
Expected: FAIL

**Step 3: Refactor Manager**

Key changes to `manager.go`:

1. Add `buffer *UtteranceBuffer` field to Manager struct
2. Initialize in `NewManager`: `buffer: NewUtteranceBuffer()`
3. Rewrite `Message()` method:

```go
func (m *Manager) Message(mr *api.MessageResponse) error {
	if len(mr.Channel.Alternatives) == 0 {
		return nil
	}

	sentence := strings.TrimSpace(mr.Channel.Alternatives[0].Transcript)
	if sentence == "" {
		return nil
	}

	// Extract words
	words := make([]transcribe.Word, 0, len(mr.Channel.Alternatives[0].Words))
	for _, word := range mr.Channel.Alternatives[0].Words {
		words = append(words, transcribe.Word{
			Speaker:        word.Speaker,
			PunctuatedWord: word.PunctuatedWord,
			Start:          word.Start,
			End:            word.End,
		})
	}

	// Interim result (not final) — broadcast for faded display
	if !mr.IsFinal {
		if m.hub != nil {
			speaker := -1
			startTime := 0.0
			if len(words) > 0 {
				if words[0].Speaker != nil {
					speaker = *words[0].Speaker
				}
				startTime = words[0].Start
			}
			m.hub.BroadcastLiveTranscriptInterim(speaker, sentence, startTime)
		}
		return nil
	}

	// Final result — buffer words
	m.buffer.AddWords(words)
	m.detector.OnSpeech()

	// If speech_final, flush buffer and persist
	if mr.SpeechFinal {
		return m.flushBuffer()
	}

	return nil
}
```

4. Add `flushBuffer()` method:

```go
func (m *Manager) flushBuffer() error {
	words := m.buffer.Flush()
	if len(words) == 0 {
		return nil
	}

	segments := transcribe.GroupWordsBySpeaker(words)
	if len(segments) == 0 {
		return nil
	}

	for i := range segments {
		segments[i].Timestamp = time.Now().UTC()
		if err := m.ensureSessionStarted(segments[i].Timestamp); err != nil {
			return err
		}

		sessionID := m.currentSession()
		if err := m.store.AppendSegment(sessionID, segments[i]); err != nil {
			return fmt.Errorf("append segment: %w", err)
		}

		if m.hub != nil {
			m.hub.BroadcastLiveTranscript(segments[i])
		}
	}
	return nil
}
```

5. Update `UtteranceEnd()` to flush buffer:

```go
func (m *Manager) UtteranceEnd(_ *api.UtteranceEndResponse) error {
	if err := m.flushBuffer(); err != nil {
		return err
	}
	m.detector.OnUtteranceEnd()
	return nil
}
```

**Step 4: Update existing tests**

Update `TestManagerLifecycle` to set `SpeechFinal: true` on the test message (since the message must now have speech_final to trigger persistence).

**Step 5: Run all tests**

Run: `go test ./internal/... -v`
Expected: PASS

**Step 6: Commit**

```
jj describe -m "feat: buffer words until speech_final for correct Deepgram streaming usage"
```

---

### Task 5: Wire Deepgram options from config

**Files:**
- Modify: `cmd/ghost-wispr/main.go`

**Step 1: Update LiveTranscriptionOptions**

In `main.go`, change the `tOptions` block (around line 311-320) to:

```go
tOptions := &interfaces.LiveTranscriptionOptions{
	Model:          "nova-2",
	Language:       "en-US",
	Diarize:        true,
	Punctuate:      true,
	SmartFormat:    true,
	Encoding:       "linear16",
	SampleRate:     selectedSampleRate,
	Channels:       1,
	Endpointing:    cfg.Transcription.Endpointing,
	InterimResults: true,
	UtteranceEndMs: cfg.Transcription.UtteranceEndMs,
	VadEvents:      true,
}
```

**Step 2: Build to verify**

Run: `go build ./cmd/ghost-wispr/`
Expected: Build succeeds

**Step 3: Commit**

```
jj describe -m "feat: wire transcription config to Deepgram streaming options"
```

---

### Task 6: Add POST /api/session/end endpoint

**Files:**
- Modify: `internal/server/server.go`
- Modify: `internal/server/api.go`
- Modify: `internal/server/api_test.go`
- Modify: `cmd/ghost-wispr/main.go`

**Step 1: Add EndSession to ControlHooks**

In `server.go`, add to `ControlHooks`:
```go
EndSession func(ctx context.Context) error
```

**Step 2: Write failing test**

In `api_test.go`, add `TestEndSession_Success` — POST to `/api/session/end` returns 204.
Add `TestEndSession_NoActiveSession` — when EndSession returns error, returns 409.

**Step 3: Run test to verify it fails**

Run: `go test ./internal/server/ -run TestEndSession -v`
Expected: FAIL

**Step 4: Add handler in api.go**

```go
mux.HandleFunc("POST /api/session/end", func(w http.ResponseWriter, r *http.Request) {
	if controls.EndSession == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "session management not available")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := controls.EndSession(ctx); err != nil {
		writeJSONError(w, http.StatusConflict, fmt.Sprintf("end session: %v", err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
})
```

**Step 5: Wire in main.go**

Add to the ControlHooks initialization:
```go
EndSession: func(ctx context.Context) error {
	return manager.ForceEndSession(ctx)
},
```

**Step 6: Run tests**

Run: `go test ./internal/server/ -v`
Expected: PASS

**Step 7: Commit**

```
jj describe -m "feat: add POST /api/session/end endpoint for manual session ending"
```

---

### Task 7: Frontend — Add interim event type and state

**Files:**
- Modify: `web/src/lib/types.ts`
- Modify: `web/src/lib/state.svelte.ts`

**Step 1: Add LiveTranscriptInterimEvent to types.ts**

```typescript
export interface LiveTranscriptInterimEvent extends BaseEvent {
  type: 'live_transcript_interim'
  speaker: number
  text: string
  start_time: number
}
```

Add to `WebSocketEvent` union type.

**Step 2: Add interim state to state.svelte.ts**

Add `interimText: string` and `interimSpeaker: number` to `AppState`.
Default: `interimText: ''`, `interimSpeaker: -1`.

Add handler in `applyEvent`:
```typescript
case 'live_transcript_interim':
  appState.interimText = event.text
  appState.interimSpeaker = event.speaker
  return
case 'live_transcript':
  appState.interimText = ''
  appState.interimSpeaker = -1
  appendLiveSegment(event)
  return
```

Clear interim in `resetState()`.

**Step 3: Commit**

```
jj describe -m "feat: add live_transcript_interim event handling to frontend state"
```

---

### Task 8: Frontend — Update LivePanel for interim display

**Files:**
- Modify: `web/src/components/LivePanel.svelte`

**Step 1: Add interimText and interimSpeaker props**

Add `interimText: string` and `interimSpeaker: number` to the component props.

**Step 2: Render interim text at bottom of live stream**

After the `{#each}` block, add:
```svelte
{#if interimText}
  <article class="segment-row interim">
    <span class="segment-time"></span>
    <strong class={`segment-speaker ${speakerClass(interimSpeaker)}`}>
      Speaker {interimSpeaker}
    </strong>
    <span class="segment-text">{interimText}</span>
  </article>
{/if}
```

**Step 3: Add CSS for interim styling**

In the component's `<style>` or global CSS, add:
```css
.segment-row.interim {
  opacity: 0.5;
  font-style: italic;
}
```

**Step 4: Wire props in App.svelte**

Pass `interimText={appState.interimText}` and `interimSpeaker={appState.interimSpeaker}` to the LivePanel component.

**Step 5: Commit**

```
jj describe -m "feat: show interim transcription as faded text in live panel (Notion-style)"
```

---

### Task 9: Frontend — Add End Session button and API

**Files:**
- Modify: `web/src/lib/api.ts`
- Modify: `web/src/components/Controls.svelte`
- Modify: `web/src/App.svelte`

**Step 1: Add endSession to api.ts**

```typescript
export function endSession(): Promise<void> {
  return request<void>('/api/session/end', { method: 'POST' })
}
```

**Step 2: Add End Session button to Controls.svelte**

Add `activeSessionId: string` and `onEndSession: () => Promise<void>` to props.

Add button after the pause/resume button:
```svelte
{#if activeSessionId}
  <button class="end-btn" type="button" onclick={handleEndSession} disabled={endBusy}>
    End Session
  </button>
{/if}
```

Add handler:
```typescript
let endBusy = $state(false)
async function handleEndSession() {
  if (endBusy) return
  endBusy = true
  try {
    await onEndSession()
  } finally {
    endBusy = false
  }
}
```

**Step 3: Wire in App.svelte**

Pass `activeSessionId={appState.activeSessionId}` and `onEndSession={endSession}` (imported from api.ts) to Controls.

**Step 4: Commit**

```
jj describe -m "feat: add End Session button with POST /api/session/end"
```

---

### Task 10: Integration test and build verification

**Step 1: Run all Go tests**

Run: `go test ./... -v`
Expected: All PASS

**Step 2: Build Go binary**

Run: `go build ./cmd/ghost-wispr/`
Expected: Build succeeds

**Step 3: Build frontend**

Run: `cd web && npm run build`
Expected: Build succeeds

**Step 4: Commit final state**

```
jj describe -m "feat: diarization quality improvements and manual session end (issues #3, #4)"
```
