# Player Improvements & Custom Dictionary Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix audio seeking when clicking far into transcripts, add 30-second skip forward/back controls, and add a custom keyword dictionary for transcription.

**Architecture:** Three independent features, each a vertical slice through the Svelte frontend and Go backend. Feature 1 (seek fix) and Feature 2 (skip controls) are frontend-only. Feature 3 (dictionary) spans config, backend Deepgram integration, config API, and settings UI.

**Tech Stack:** Svelte 5 + TypeScript (frontend), Go 1.25+ (backend), Deepgram API (transcription), SQLite (config persistence), Vite (build), vitest (frontend tests), `go test` (backend tests)

**Source:** [Slack message from Peter](https://trajectorylabs.slack.com/archives/C0A5WLJS006/p1775706184332929) — items 2, 3, and 4.

**Test commands:**
- Backend: `go test ./...`
- Frontend: `cd web && npm test`
- Dev server: `cd web && npm run dev` (proxies API to :8080)

---

## Feature 1: Fix Transcript Seek (Bug)

**Bug report:** "If I click far into the transcript, it starts the recording again at 0 seconds."

**Root cause hypothesis:** The audio element uses `preload="metadata"`, so the browser only downloads headers initially. When `seekTo()` sets `audioEl.currentTime` to a position far into the file, the browser must issue a Range request. If the seek target exceeds the buffered/seekable range and the browser can't resolve the byte offset (e.g., VBR MP3 without proper Xing header, or a timing race), it silently resets `currentTime` to 0. Meanwhile, `onTimeUpdate` faithfully reports 0 to the UI. There is no `onseeking`/`onseeked` handler, no validation that the seek succeeded, and no loading indicator during the seek.

**Uncertainty:** The exact failure mode needs empirical confirmation. The plan handles both "seek silently fails" and "seek is slow and UI shows stale time" scenarios.

### Task 1.1: Add seek state tracking and event handlers to AudioPlayer

**Files:**
- Modify: `web/src/components/AudioPlayer.svelte`
- Modify: `web/src/components/__tests__/AudioPlayer.test.ts`

- [ ] **Step 1: Write failing tests for seek behavior**

Add tests that verify: (a) clicking a transcript line while audio is loaded triggers seek to the correct time, (b) a `seeking` CSS class is applied during seek, (c) `onTimeUpdate` does not overwrite `currentTime` while seeking.

```typescript
it('applies seeking class during seek', async () => {
  const segments = [
    { start_time: 0, end_time: 30, text: 'Hello', speaker: 0, timestamp: '2026-01-01T00:00:00Z' },
    { start_time: 300, end_time: 330, text: 'Far segment', speaker: 0, timestamp: '2026-01-01T00:05:00Z' },
  ]
  const { container } = render(AudioPlayer, { props: { sessionId: 's1', segments } })
  const audio = container.querySelector('audio') as HTMLAudioElement

  // Simulate metadata loaded
  Object.defineProperty(audio, 'duration', { value: 600, writable: true })
  await audio.dispatchEvent(new Event('loadedmetadata'))

  // Click the far segment
  const lines = container.querySelectorAll('.line')
  await fireEvent.click(lines[1])

  // Should show seeking state
  expect(container.querySelector('.audio-player')?.classList.contains('seeking')).toBe(true)

  // Simulate seeked
  await audio.dispatchEvent(new Event('seeked'))
  expect(container.querySelector('.audio-player')?.classList.contains('seeking')).toBe(false)
})
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd web && npx vitest run src/components/__tests__/AudioPlayer.test.ts`
Expected: FAIL — no `seeking` class behavior exists yet.

- [ ] **Step 3: Add seeking state and event handlers**

Modify `AudioPlayer.svelte` to:
1. Add a `seeking` reactive state variable
2. Add `onseeking` handler that sets `seeking = true`
3. Add `onseeked` handler that sets `seeking = false` and syncs `currentTime`
4. Guard `onTimeUpdate` to skip updates while `seeking` is true
5. Apply `seeking` CSS class to the player container
6. In `seekTo()`, also trigger play if audio was already playing (to force the browser to load audio at the seek target)

```svelte
<script lang="ts">
  // ... existing code ...
  let seeking = $state(false)

  function seekTo(seconds: number) {
    if (!audioEl) return
    seeking = true
    audioEl.currentTime = seconds
    setActiveAudioSession(sessionId)
  }

  function onSeeking() {
    seeking = true
  }

  function onSeeked() {
    if (!audioEl) return
    seeking = false
    currentTime = audioEl.currentTime
  }

  function onTimeUpdate() {
    if (!audioEl || seeking) return
    currentTime = audioEl.currentTime
  }
</script>

<div class="audio-player" class:seeking data-testid="audio-player">
  <audio
    bind:this={audioEl}
    src={`/api/sessions/${encodeURIComponent(sessionId)}/audio`}
    preload="auto"
    onloadedmetadata={onLoadedMetadata}
    ontimeupdate={onTimeUpdate}
    onseeking={onSeeking}
    onseeked={onSeeked}
    onplay={() => (playing = true)}
    onpause={() => (playing = false)}
    onerror={() => {
      loading = false
      error = 'Audio unavailable'
    }}
  ></audio>
  <!-- ... -->
</div>
```

Key change: `preload="metadata"` → `preload="auto"`. This ensures the browser downloads the full file, making seeks reliable. These are meeting recordings served from local storage, so bandwidth isn't a concern.

- [ ] **Step 4: Add seeking indicator styling**

In `web/src/app.css`, add a subtle pulsing opacity to the player when seeking:

```css
.audio-player.seeking .audio-time {
  opacity: 0.5;
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `cd web && npx vitest run src/components/__tests__/AudioPlayer.test.ts`
Expected: PASS

- [ ] **Step 6: Describe and advance**

```bash
jj describe -m "fix: handle audio seeking state to prevent transcript click resetting to 0s

Add onseeking/onseeked handlers, guard onTimeUpdate during seeks, switch to
preload=auto for reliable seeking of local audio files."
jj new
```

---

## Feature 2: Skip Forward/Back 30 Seconds

### Task 2.1: Add skip buttons to AudioPlayer

**Files:**
- Modify: `web/src/components/AudioPlayer.svelte`
- Modify: `web/src/components/__tests__/AudioPlayer.test.ts`
- Modify: `web/src/app.css`

- [ ] **Step 1: Write failing tests for skip controls**

```typescript
it('skip back 30s button decrements currentTime', async () => {
  const segments = [
    { start_time: 0, end_time: 120, text: 'Long segment', speaker: 0, timestamp: '2026-01-01T00:00:00Z' },
  ]
  const { container } = render(AudioPlayer, { props: { sessionId: 's1', segments } })
  const audio = container.querySelector('audio') as HTMLAudioElement
  Object.defineProperty(audio, 'duration', { value: 600, writable: true })
  await audio.dispatchEvent(new Event('loadedmetadata'))

  // Simulate playback at 90s
  Object.defineProperty(audio, 'currentTime', { value: 90, writable: true, configurable: true })
  await audio.dispatchEvent(new Event('timeupdate'))

  const skipBack = container.querySelector('[data-testid="skip-back"]') as HTMLButtonElement
  expect(skipBack).toBeTruthy()
  await fireEvent.click(skipBack)

  // Should have set currentTime to 60
  expect(audio.currentTime).toBe(60)
})

it('skip forward 30s button increments currentTime', async () => {
  const segments = [
    { start_time: 0, end_time: 120, text: 'Long segment', speaker: 0, timestamp: '2026-01-01T00:00:00Z' },
  ]
  const { container } = render(AudioPlayer, { props: { sessionId: 's1', segments } })
  const audio = container.querySelector('audio') as HTMLAudioElement
  Object.defineProperty(audio, 'duration', { value: 600, writable: true })
  await audio.dispatchEvent(new Event('loadedmetadata'))

  Object.defineProperty(audio, 'currentTime', { value: 90, writable: true, configurable: true })
  await audio.dispatchEvent(new Event('timeupdate'))

  const skipFwd = container.querySelector('[data-testid="skip-forward"]') as HTMLButtonElement
  expect(skipFwd).toBeTruthy()
  await fireEvent.click(skipFwd)

  expect(audio.currentTime).toBe(120)
})

it('skip back clamps to 0', async () => {
  const segments = [
    { start_time: 0, end_time: 120, text: 'Segment', speaker: 0, timestamp: '2026-01-01T00:00:00Z' },
  ]
  const { container } = render(AudioPlayer, { props: { sessionId: 's1', segments } })
  const audio = container.querySelector('audio') as HTMLAudioElement
  Object.defineProperty(audio, 'duration', { value: 600, writable: true })
  await audio.dispatchEvent(new Event('loadedmetadata'))

  Object.defineProperty(audio, 'currentTime', { value: 10, writable: true, configurable: true })
  await audio.dispatchEvent(new Event('timeupdate'))

  const skipBack = container.querySelector('[data-testid="skip-back"]') as HTMLButtonElement
  await fireEvent.click(skipBack)

  expect(audio.currentTime).toBe(0)
})
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd web && npx vitest run src/components/__tests__/AudioPlayer.test.ts`
Expected: FAIL — no skip buttons exist.

- [ ] **Step 3: Add skip functions and buttons**

In `AudioPlayer.svelte`, add:

```typescript
function skipBack() {
  if (!audioEl) return
  audioEl.currentTime = Math.max(0, audioEl.currentTime - 30)
  setActiveAudioSession(sessionId)
}

function skipForward() {
  if (!audioEl) return
  audioEl.currentTime = Math.min(duration, audioEl.currentTime + 30)
  setActiveAudioSession(sessionId)
}
```

Update the controls section (between the play button and time display):

```svelte
<div class="audio-controls">
  <button type="button" class="audio-btn" onclick={skipBack} data-testid="skip-back" title="Back 30s">
    -30s
  </button>
  <button type="button" class="audio-btn" onclick={togglePlay}>
    {playing ? 'Pause' : 'Play'}
  </button>
  <button type="button" class="audio-btn" onclick={skipForward} data-testid="skip-forward" title="Forward 30s">
    +30s
  </button>
  <span class="audio-time">{prettyTime(currentTime)} / {prettyTime(duration)}</span>
</div>
```

Note: also shorten the play/pause label from "Play Audio"/"Pause Audio" to "Play"/"Pause" since the button context is now clearer with adjacent skip controls. Update the existing test in `AudioPlayer.test.ts` line 24 that checks for `'Play Audio'` to use `'Play'` instead.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd web && npx vitest run src/components/__tests__/AudioPlayer.test.ts`
Expected: PASS

- [ ] **Step 5: Verify existing tests still pass**

Run: `cd web && npx vitest run`
Expected: All tests PASS

- [ ] **Step 6: Describe and advance**

```bash
jj describe -m "feat: add 30-second skip forward/back controls to audio player"
jj new
```

---

## Feature 3: Custom Dictionary (Deepgram Keywords)

Deepgram supports a `keywords` parameter on both live (WebSocket) and batch (REST) transcription that boosts recognition of specific terms. The SDK already has the field: `Keywords []string` on `LiveTranscriptionOptions` (line 27 of `types-stream.go`). The batch REST endpoint accepts `keywords` as a query parameter.

Format: each keyword string can optionally include an intensity boost, e.g. `"Taiga:2"` or just `"Anthropic"`. Default boost is 1.5.

### Task 3.1: Add Keywords to config (backend)

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/store.go`
- Modify: `internal/config/store_test.go`

- [ ] **Step 1: Write failing test for keywords in config**

```go
func TestStore_Update_KeywordsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "config.yaml")
	s, _, err := NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	s.path = yamlPath

	err = s.Update(func(c *Config) {
		c.Transcription.Keywords = []string{"Taiga", "Anthropic", "Lyon"}
	})
	if err != nil {
		t.Fatal(err)
	}

	loaded, _, err := NewStore(yamlPath)
	if err != nil {
		t.Fatal(err)
	}
	got := loaded.Get().Transcription.Keywords
	if len(got) != 3 || got[0] != "Taiga" || got[1] != "Anthropic" || got[2] != "Lyon" {
		t.Fatalf("keywords round-trip failed: got %v", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run TestStore_Update_KeywordsRoundTrip -v`
Expected: FAIL — `Keywords` field doesn't exist on `Transcription`.

- [ ] **Step 3: Add Keywords field to Transcription config**

In `internal/config/config.go`, add the `Keywords` field to the `Transcription` struct:

```go
type Transcription struct {
	Endpointing    string   `yaml:"endpointing"`
	UtteranceEndMs string   `yaml:"utterance_end_ms"`
	Keywords       []string `yaml:"keywords"`
}
```

Also update `copyConfig()` in `store.go` to deep-copy the keywords slice:

```go
func (s *Store) copyConfig() Config {
	c := s.cfg

	if s.cfg.MicSampleRates != nil {
		c.MicSampleRates = make([]int, len(s.cfg.MicSampleRates))
		copy(c.MicSampleRates, s.cfg.MicSampleRates)
	}

	if s.cfg.Transcription.Keywords != nil {
		c.Transcription.Keywords = make([]string, len(s.cfg.Transcription.Keywords))
		copy(c.Transcription.Keywords, s.cfg.Transcription.Keywords)
	}

	// ... existing preset copy ...
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/config/ -run TestStore_Update_KeywordsRoundTrip -v`
Expected: PASS

- [ ] **Step 5: Describe and advance**

```bash
jj describe -m "feat: add keywords field to transcription config"
jj new
```

### Task 3.2: Pass Keywords to Deepgram live transcription

**Files:**
- Modify: `cmd/ghost-wispr/main.go` (~line 1432)

- [ ] **Step 1: Add keywords to LiveTranscriptionOptions**

In `main.go`, where `tOptions` is constructed (line 1432), add the keywords:

```go
tOptions := &interfaces.LiveTranscriptionOptions{
	Model:          cfg.DeepgramModel,
	Language:       "en-US",
	Diarize:        true,
	Punctuate:      true,
	SmartFormat:    true,
	Encoding:       "linear16",
	SampleRate:     sampleRate,
	Channels:       1,
	Endpointing:    cfg.Transcription.Endpointing,
	InterimResults: true,
	UtteranceEndMs: cfg.Transcription.UtteranceEndMs,
	VadEvents:      true,
	Keywords:       cfg.Transcription.Keywords,
}
```

- [ ] **Step 2: Verify build succeeds**

Run: `go build ./cmd/ghost-wispr/`
Expected: Build succeeds with no errors.

- [ ] **Step 3: Describe and advance**

```bash
jj describe -m "feat: pass keywords from config to Deepgram live transcription"
jj new
```

### Task 3.3: Pass Keywords to Deepgram batch transcription

**Files:**
- Modify: `internal/transcribe/batch.go`
- Modify: `internal/transcribe/batch_test.go`

- [ ] **Step 1: Write failing test for keywords in batch request**

```go
func TestDeepgramBatchTranscriber_Keywords(t *testing.T) {
	expectedKeywords := []string{"Taiga", "Anthropic"}
	var capturedURL string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedURL = r.URL.String()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"results":{"channels":[{"alternatives":[{"transcript":"test"}]}]}}`)
	}))
	defer ts.Close()

	tmpDir := t.TempDir()
	audioPath := filepath.Join(tmpDir, "session.mp3")
	if err := os.WriteFile(audioPath, []byte("fake"), 0o644); err != nil {
		t.Fatal(err)
	}

	transcriber := NewDeepgramBatchTranscriber(DeepgramBatchConfig{
		APIKey:     "test-key",
		Model:      "nova-3",
		BaseURL:    ts.URL,
		Keywords:   expectedKeywords,
		HTTPClient: ts.Client(),
	})

	_, err := transcriber.Transcribe(context.Background(), audioPath)
	if err != nil {
		t.Fatal(err)
	}

	parsed, _ := url.Parse(capturedURL)
	gotKeywords := parsed.Query()["keywords"]
	if len(gotKeywords) != 2 || gotKeywords[0] != "Taiga" || gotKeywords[1] != "Anthropic" {
		t.Fatalf("expected keywords [Taiga Anthropic], got %v", gotKeywords)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/transcribe/ -run TestDeepgramBatchTranscriber_Keywords -v`
Expected: FAIL — `Keywords` field doesn't exist on `DeepgramBatchConfig`.

- [ ] **Step 3: Add Keywords to batch transcriber**

In `internal/transcribe/batch.go`:

1. Add `Keywords` field to `DeepgramBatchConfig`:

```go
type DeepgramBatchConfig struct {
	APIKey     string
	Model      string
	BaseURL    string
	Keywords   []string
	HTTPClient *http.Client
}
```

2. Add `keywords` field to `deepgramBatchTranscriber`:

```go
type deepgramBatchTranscriber struct {
	apiKey     string
	model      string
	baseURL    string
	keywords   []string
	httpClient *http.Client
}
```

3. In `NewDeepgramBatchTranscriber`, copy keywords:

```go
return &deepgramBatchTranscriber{
	apiKey:     strings.TrimSpace(cfg.APIKey),
	model:      strings.TrimSpace(cfg.Model),
	baseURL:    strings.TrimRight(baseURL, "/"),
	keywords:   cfg.Keywords,
	httpClient: httpClient,
}
```

4. In `Transcribe()`, add keywords as query params (each keyword is a separate `keywords` param, per Deepgram API):

```go
q := u.Query()
if d.model != "" {
	q.Set("model", d.model)
}
q.Set("smart_format", "true")
q.Set("punctuate", "true")
for _, kw := range d.keywords {
	q.Add("keywords", kw)
}
u.RawQuery = q.Encode()
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/transcribe/ -run TestDeepgramBatchTranscriber_Keywords -v`
Expected: PASS

- [ ] **Step 5: Wire keywords through in main.go**

In `cmd/ghost-wispr/main.go`, where `makeBatchTranscriber` creates the `DeepgramBatchConfig`, pass keywords from config. Find the `makeBatchTranscriber` function and update it:

```go
// In the deepgram case of makeBatchTranscriber:
return transcribe.NewDeepgramBatchTranscriber(transcribe.DeepgramBatchConfig{
	APIKey:   cfg.DeepgramAPIKey,
	Model:    cfg.BatchTranscription.Model,
	Keywords: cfg.Transcription.Keywords,
}), nil
```

- [ ] **Step 6: Run all backend tests**

Run: `go test ./...`
Expected: All tests PASS.

- [ ] **Step 7: Describe and advance**

```bash
jj describe -m "feat: pass keywords to Deepgram batch transcription"
jj new
```

### Task 3.4: Expose Keywords in config API

**Files:**
- Modify: `internal/server/api.go`
- Modify: `internal/server/api_test.go`
- Modify: `web/src/lib/config-api.ts`

- [ ] **Step 1: Write failing test for keywords in config API**

In `internal/server/api_test.go`:

```go
func TestPatchConfig_Keywords(t *testing.T) {
	cfgStore := newTestConfigStore(t)
	h, err := Handler(testStaticFS(t), NewHub(), &apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
	}, &ControlHooks{}, "", nil, cfgStore)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	body := `{"transcription":{"keywords":["Taiga","Anthropic","Lyon"]}}`
	req := httptest.NewRequest(http.MethodPatch, "/api/config", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	trans := resp["transcription"].(map[string]any)
	keywords := trans["keywords"].([]any)
	if len(keywords) != 3 {
		t.Fatalf("expected 3 keywords, got %v", keywords)
	}

	// Verify persisted
	got := cfgStore.Get().Transcription.Keywords
	if len(got) != 3 || got[0] != "Taiga" {
		t.Fatalf("keywords not persisted: %v", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/server/ -run TestPatchConfig_Keywords -v`
Expected: FAIL — `Keywords` not handled in config patch.

- [ ] **Step 3: Add Keywords to config API**

In `api.go`:

1. Add `Keywords` field to `transcriptionPatch`:

```go
type transcriptionPatch struct {
	Endpointing    *string   `json:"endpointing,omitempty"`
	UtteranceEndMs *string   `json:"utterance_end_ms,omitempty"`
	Keywords       *[]string `json:"keywords,omitempty"`
}
```

2. In `applyConfigPatch`, handle keywords:

```go
if p.Transcription != nil {
	if p.Transcription.Endpointing != nil {
		c.Transcription.Endpointing = *p.Transcription.Endpointing
	}
	if p.Transcription.UtteranceEndMs != nil {
		c.Transcription.UtteranceEndMs = *p.Transcription.UtteranceEndMs
	}
	if p.Transcription.Keywords != nil {
		c.Transcription.Keywords = *p.Transcription.Keywords
	}
}
```

3. Add `Keywords` field to `configTranscriptionResponse` struct (line ~1437 in `api.go`):

```go
type configTranscriptionResponse struct {
	Endpointing    string   `json:"endpointing"`
	UtteranceEndMs string   `json:"utterance_end_ms"`
	Keywords       []string `json:"keywords"`
}
```

4. In `handleGetConfig` (line ~1465), pass keywords when building the response:

```go
Transcription: configTranscriptionResponse{
	Endpointing:    cfg.Transcription.Endpointing,
	UtteranceEndMs: cfg.Transcription.UtteranceEndMs,
	Keywords:       cfg.Transcription.Keywords,
},
```

4. In `web/src/lib/config-api.ts`, update the `ConfigResponse` type:

```typescript
transcription: {
  endpointing: string
  utterance_end_ms: string
  keywords: string[]
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/server/ -run TestPatchConfig_Keywords -v`
Expected: PASS

- [ ] **Step 5: Run all backend tests**

Run: `go test ./...`
Expected: All tests PASS.

- [ ] **Step 6: Describe and advance**

```bash
jj describe -m "feat: expose transcription keywords in config API"
jj new
```

### Task 3.5: Add Keywords UI in Settings

**Files:**
- Modify: `web/src/components/GeneralSettings.svelte`

- [ ] **Step 1: Add keywords editor to GeneralSettings**

Add a textarea-based keyword editor to `GeneralSettings.svelte`. Keywords are displayed as a comma-separated list for simplicity. Add state tracking and include keywords in the save patch.

```svelte
<script lang="ts">
  // ... existing imports and state ...

  let keywords = $state((config.transcription.keywords ?? []).join(', '))

  $effect(() => {
    if (configState.config) {
      // ... existing syncs ...
      keywords = (configState.config.transcription.keywords ?? []).join(', ')
    }
  })

  function parseKeywords(raw: string): string[] {
    return raw
      .split(',')
      .map((k) => k.trim())
      .filter((k) => k.length > 0)
  }

  async function save(): Promise<void> {
    saving = true
    feedback = ''
    try {
      const patch: Record<string, unknown> = {}

      // ... existing patch logic ...

      const parsedKeywords = parseKeywords(keywords)
      const currentKeywords = config.transcription.keywords ?? []
      if (JSON.stringify(parsedKeywords) !== JSON.stringify(currentKeywords)) {
        // Merge with existing transcription patch if any
        const transPatch = (patch.transcription as Record<string, unknown>) ?? {}
        transPatch.keywords = parsedKeywords
        patch.transcription = transPatch
      }

      // ... rest of save logic ...
    }
  }
</script>
```

Add the UI field between utterance end and the save button:

```svelte
<div class="field">
  <label for="keywords">Custom Dictionary (Keywords)</label>
  <textarea
    id="keywords"
    bind:value={keywords}
    placeholder="Taiga, Anthropic, Lyon"
    rows="3"
  ></textarea>
  <span class="hint">Comma-separated terms to boost in transcription. Takes effect on next recording.</span>
</div>
```

Add textarea styling in the `<style>` block:

```css
.field textarea {
  padding: 0.5rem;
  border: 1px solid var(--line);
  border-radius: 6px;
  font-family: var(--font-mono);
  font-size: 0.9rem;
  background: var(--panel);
  resize: vertical;
}
```

- [ ] **Step 2: Run frontend tests**

Run: `cd web && npx vitest run`
Expected: All tests PASS. (No new tests needed — the save flow is already tested via existing GeneralSettings patterns, and the change is purely additive.)

- [ ] **Step 3: Describe and advance**

```bash
jj describe -m "feat: add custom dictionary (keywords) UI to settings"
jj new
```

---

## Notes

### Feature 1 — Open question
Switching from `preload="metadata"` to `preload="auto"` is the most reliable fix for local audio. If recordings get very large (multi-hour), we may want to revisit with lazy loading + robust seek fallback. For now, local network latency is ~0 so this is fine.

### Feature 3 — Keywords take effect on next recording
Deepgram keywords are set at connection time (WebSocket) or request time (batch). Changing keywords via the settings UI will:
- **Live transcription:** Take effect on next Deepgram WebSocket connection (next recording start, or reconnect). The current `OnChange` handler doesn't reconnect the WebSocket for transcription setting changes — this is an existing limitation that also applies to endpointing/utterance_end_ms changes.
- **Batch transcription:** Take effect immediately since the batch transcriber is recreated in `OnChange`.

This is acceptable because keywords are a "set once" configuration, not something users toggle mid-recording.

### Item 1 from Peter's message (lapel mics)
Not addressed here — this is a hardware/setup concern. Consider documenting recommended mic setups (lapel mics, Bluetooth options) in user-facing docs as a separate task.
