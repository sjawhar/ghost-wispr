# Mic Reconnection Resilience

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Make ghost-wispr automatically recover from USB audio device disconnects instead of silently dying.

**Architecture:** Store mic construction parameters to enable reopening, add exponential backoff retry with device reconnection to the streaming loop, reset backoff on successful reopen. No max retry limit — the process retries indefinitely (30s ceiling) since systemd restart doesn't help if the device is physically gone.

**Tech Stack:** Go 1.25, PortAudio (gordonklaus/portaudio), existing `micStreamer` interface

---

## Problem

When the Jabra USB speaker disconnects (USB hub glitch, cable bump, etc.), the ALSA device node is deleted. The `Mic.Stream()` call returns an error, and `streamMicWithRetry()` logs it once and **exits the goroutine silently**. The process stays alive (HTTP server continues) but transcription is permanently dead until manual service restart.

The existing code only retries on "overflow" errors:

```go
// cmd/ghost-wispr/main.go:648-655 — current behavior
if strings.Contains(strings.ToLower(err.Error()), "overflow") {
    logf("warning: mic input overflow, restarting stream")
    wait(250 * time.Millisecond)
    continue
}
logf("mic stream error: %v", err)
return  // ← goroutine exits, transcription dead forever
```

## Design

### Layer 1: Mic device reopening

`Mic` currently discards its construction parameters after `NewMic()`. Store `sampleRate` and `framesPerBuffer` so we can close the dead stream and open a fresh one pointing to the new device node.

New method on `Mic`:

```go
func (m *Mic) Reopen() error {
    _ = m.stream.Stop()
    _ = m.stream.Close()
    stream, err := portaudio.OpenDefaultStream(1, 0, float64(m.sampleRate), m.framesPerBuffer, m.buf)
    if err != nil {
        return err
    }
    m.stream = stream
    return m.stream.Start()
}
```

### Layer 2: Retry all errors with backoff

`streamMicWithRetry` changes:
- On **overflow**: same as today (250ms wait, no reopen needed)
- On **any other error**: wait with exponential backoff (1s → 2s → 4s → ... → 30s cap), call `Reopen()`, then retry `Stream()`
- On **reopen failure**: skip `Stream()`, increase backoff, try `Reopen()` again next iteration
- On **successful reopen**: reset backoff to 1s
- On **context cancellation**: exit cleanly (same as today)

The `micStreamer` interface gains a `Reopen() error` method.

### What we're NOT doing

- **Deepgram reconnection**: If Deepgram's websocket dies independently, that's a separate failure mode. The keep-alive is enabled (`EnableKeepAlive: true`), which should handle brief mic outages. For prolonged outages where Deepgram times out, the write-side error will trigger the same retry loop — the mic reopens fine, the write fails, we retry. Acceptable for now; Deepgram reconnection can be a follow-up.
- **Crash-on-max-retries**: The process retries indefinitely. systemd restart wouldn't help if the device is physically removed, and the 30s polling interval is acceptable for a background service.
- **Error source discrimination**: We don't distinguish mic-read errors from writer errors. Both trigger the same retry path. Reopening the mic when the writer failed is a no-op (the old stream is still fine), and Stream will quickly re-error from the writer, triggering the next retry.

---

## Implementation

### Task 1: Store mic construction parameters and add Reopen

**Files:**
- Modify: `internal/audio/mic.go`

**Step 1: Add fields and update constructor**

Add `sampleRate` and `framesPerBuffer` to the `Mic` struct. Update `NewMic` to store them.

```go
type Mic struct {
	stream         *portaudio.Stream
	buf            []int16
	sampleRate     int
	framesPerBuffer int
}

func NewMic(sampleRate, framesPerBuffer int) (*Mic, error) {
	buf := make([]int16, framesPerBuffer)
	stream, err := portaudio.OpenDefaultStream(1, 0, float64(sampleRate), framesPerBuffer, buf)
	if err != nil {
		return nil, err
	}
	return &Mic{stream: stream, buf: buf, sampleRate: sampleRate, framesPerBuffer: framesPerBuffer}, nil
}
```

**Step 2: Add Reopen method**

```go
// Reopen closes the current PortAudio stream and opens a fresh one with the
// same parameters. This picks up new device nodes after a USB reconnect.
func (m *Mic) Reopen() error {
	_ = m.stream.Stop()
	_ = m.stream.Close()
	stream, err := portaudio.OpenDefaultStream(1, 0, float64(m.sampleRate), m.framesPerBuffer, m.buf)
	if err != nil {
		return err
	}
	m.stream = stream
	return m.stream.Start()
}
```

**Step 3: Run tests**

Run: `go test ./internal/audio/ -v`
Expected: PASS (existing recorder tests unaffected; Mic has no unit tests since it requires hardware)

**Step 4: Describe and advance**

```
jj describe -m "feat(audio): store mic params and add Reopen for device reconnection"
jj new
```

---

### Task 2: Update streamMicWithRetry to retry device errors

**Files:**
- Modify: `cmd/ghost-wispr/main.go`
- Modify: `cmd/ghost-wispr/main_test.go`

**Step 1: Update micStreamer interface**

In `main.go`, add `Reopen` to the interface:

```go
type micStreamer interface {
	Stream(writer io.Writer) error
	Reopen() error
}
```

**Step 2: Update fakeStreamer in main_test.go**

```go
type fakeStreamer struct {
	errs       []error
	reopenErrs []error
	calls      int
	reopens    int
}

func (f *fakeStreamer) Stream(_ io.Writer) error {
	f.calls++
	if len(f.errs) == 0 {
		return nil
	}
	err := f.errs[0]
	f.errs = f.errs[1:]
	return err
}

func (f *fakeStreamer) Reopen() error {
	f.reopens++
	if len(f.reopenErrs) == 0 {
		return nil
	}
	err := f.reopenErrs[0]
	f.reopenErrs = f.reopenErrs[1:]
	return err
}
```

**Step 3: Write new tests (replace TestStreamMicWithRetryStopsOnNonOverflow)**

Replace the existing `StopsOnNonOverflow` test with tests for the new behavior:

```go
func TestStreamMicWithRetryRetriesDeviceError(t *testing.T) {
	// Device error → reopen succeeds → stream again → succeeds
	streamer := &fakeStreamer{errs: []error{errors.New("device disconnected"), nil}}
	var waits []time.Duration
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	streamMicWithRetry(ctx, streamer, bytes.NewBuffer(nil), func(d time.Duration) {
		waits = append(waits, d)
	}, func(string, ...any) {})

	if streamer.calls != 2 {
		t.Fatalf("expected 2 stream calls, got %d", streamer.calls)
	}
	if streamer.reopens != 1 {
		t.Fatalf("expected 1 reopen call, got %d", streamer.reopens)
	}
	if len(waits) != 1 || waits[0] != time.Second {
		t.Fatalf("expected 1 wait of 1s, got %v", waits)
	}
}

func TestStreamMicWithRetryBacksOffOnReopenFailure(t *testing.T) {
	// Device error → reopen fails → backoff increases → reopen fails → backoff increases → ctx cancel
	streamer := &fakeStreamer{
		errs:       []error{errors.New("ENODEV"), errors.New("ENODEV"), errors.New("ENODEV")},
		reopenErrs: []error{errors.New("no device"), errors.New("no device"), errors.New("no device")},
	}
	var waits []time.Duration
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		// Cancel after the third wait to stop the loop
		for len(waits) < 3 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()

	streamMicWithRetry(ctx, streamer, bytes.NewBuffer(nil), func(d time.Duration) {
		waits = append(waits, d)
	}, func(string, ...any) {})

	// First stream error: 1s wait, reopen fails, backoff doubles
	// Second: skip stream (reopen failed), 2s wait, reopen fails, backoff doubles
	// Third: skip stream, 4s wait, reopen fails
	// Then ctx cancelled
	if waits[0] != time.Second {
		t.Fatalf("expected first wait 1s, got %v", waits[0])
	}
	if waits[1] != 2*time.Second {
		t.Fatalf("expected second wait 2s, got %v", waits[1])
	}
	if waits[2] != 4*time.Second {
		t.Fatalf("expected third wait 4s, got %v", waits[2])
	}
}

func TestStreamMicWithRetryResetsBackoffOnSuccessfulReopen(t *testing.T) {
	// Error → reopen fails (backoff grows) → reopen succeeds (backoff resets) → error → reopen succeeds
	streamer := &fakeStreamer{
		errs:       []error{errors.New("err1"), errors.New("err2"), nil},
		reopenErrs: []error{errors.New("no device"), nil},
	}
	var waits []time.Duration
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	streamMicWithRetry(ctx, streamer, bytes.NewBuffer(nil), func(d time.Duration) {
		waits = append(waits, d)
	}, func(string, ...any) {})

	// err1 → 1s wait, reopen fails → 2s wait, reopen succeeds (reset) → err2 → 1s wait, reopen succeeds → stream succeeds
	if len(waits) != 3 {
		t.Fatalf("expected 3 waits, got %d: %v", len(waits), waits)
	}
	if waits[0] != time.Second {
		t.Fatalf("expected first wait 1s, got %v", waits[0])
	}
	if waits[1] != 2*time.Second {
		t.Fatalf("expected second wait 2s, got %v", waits[1])
	}
	if waits[2] != time.Second {
		t.Fatalf("expected third wait 1s (reset), got %v", waits[2])
	}
}

func TestStreamMicWithRetryOverflowDoesNotReopen(t *testing.T) {
	// Overflow should NOT trigger reopen — just a brief wait and retry
	streamer := &fakeStreamer{errs: []error{errors.New("Input overflowed"), nil}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	streamMicWithRetry(ctx, streamer, bytes.NewBuffer(nil), func(_ time.Duration) {}, func(string, ...any) {})

	if streamer.reopens != 0 {
		t.Fatalf("expected 0 reopen calls for overflow, got %d", streamer.reopens)
	}
}
```

**Step 4: Run tests to verify they fail**

Run: `go test ./cmd/ghost-wispr/ -v`
Expected: FAIL (old `streamMicWithRetry` exits on device errors instead of retrying)

**Step 5: Implement new streamMicWithRetry**

```go
func streamMicWithRetry(
	ctx context.Context,
	streamer micStreamer,
	writer io.Writer,
	wait func(time.Duration),
	logf func(string, ...any),
) {
	const (
		overflowWait = 250 * time.Millisecond
		baseBackoff  = time.Second
		maxBackoff   = 30 * time.Second
	)

	backoff := baseBackoff

	for {
		if ctx.Err() != nil {
			return
		}

		err := streamer.Stream(writer)
		if err == nil || ctx.Err() != nil {
			return
		}

		if strings.Contains(strings.ToLower(err.Error()), "overflow") {
			logf("warning: mic input overflow, restarting stream")
			wait(overflowWait)
			continue
		}

		logf("mic stream error (retrying in %v): %v", backoff, err)
		wait(backoff)

		if ctx.Err() != nil {
			return
		}

		if reopenErr := streamer.Reopen(); reopenErr != nil {
			logf("mic reopen failed: %v", reopenErr)
			backoff = min(backoff*2, maxBackoff)
			continue
		}

		logf("mic reopened successfully")
		backoff = baseBackoff
	}
}
```

**Step 6: Run tests to verify they pass**

Run: `go test ./cmd/ghost-wispr/ -v`
Expected: PASS

**Step 7: Run full test suite**

Run: `go test ./...`
Expected: PASS

**Step 8: Describe and advance**

```
jj describe -m "feat: retry mic stream on device errors with exponential backoff and reopen"
jj new
```

---

### Task 3: Build, deploy, and verify

**Files:**
- None (build/deploy only)

**Step 1: Build**

Run: `make build`
Expected: Binary compiles successfully

**Step 2: Deploy**

Run: `make deploy`
Expected: Service restarts, `curl localhost:8080/api/version` returns version info

**Step 3: Verify process is healthy**

Run: `systemctl --user status ghost-wispr.service`
Expected: Active (running)

Check audio device is open:
Run: `ls -la /proc/$(pgrep ghost-wispr)/fd/ | grep snd`
Expected: Shows `/dev/snd/pcmC2D0c` (not deleted)

**Step 4: Describe and advance**

```
jj describe -m "chore: build and deploy mic reconnection fix"
jj new
```
