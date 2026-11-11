package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/server"
)

type fakeStreamer struct {
	errs       []error
	reopenErrs []error
	calls      int
	reopens    int
	stops      int
	stopped    bool
}

func (f *fakeStreamer) Stream(_ io.Writer) error {
	f.calls++
	if f.stopped {
		return errors.New("microphone stopped")
	}
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

func (f *fakeStreamer) Start() error { return nil }

func (f *fakeStreamer) Stop() error {
	f.stops++
	f.stopped = true
	return nil
}

func TestStreamMicWithRetryRetriesOverflow(t *testing.T) {
	streamer := &fakeStreamer{errs: []error{errors.New("Input overflowed"), nil}}
	var waits int
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	streamMicWithRetry(ctx, newCaptureGate(nil, nil, nil), streamer, bytes.NewBuffer(nil), func(_ time.Duration) {
		waits++
	}, func(string, ...any) {}, nil)

	if streamer.calls != 2 {
		t.Fatalf("expected 2 stream calls, got %d", streamer.calls)
	}
	if waits != 1 {
		t.Fatalf("expected 1 wait call, got %d", waits)
	}
	if streamer.reopens != 1 {
		t.Fatalf("expected 1 reopen call for overflow, got %d", streamer.reopens)
	}
}

func TestStreamMicWithRetryOverflowReopens(t *testing.T) {
	streamer := &fakeStreamer{errs: []error{errors.New("Input overflowed"), nil}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	streamMicWithRetry(ctx, newCaptureGate(nil, nil, nil), streamer, bytes.NewBuffer(nil), func(_ time.Duration) {}, func(string, ...any) {}, nil)

	if streamer.reopens != 1 {
		t.Fatalf("expected 1 reopen call for overflow, got %d", streamer.reopens)
	}
}

// TestStreamMicWithRetryRequiresRedBeforeEachReopen fails if either reconnect
// path starts microphone capture after the light refuses confirmation, or
// retries a stopped device without another confirmation.
func TestStreamMicWithRetryRequiresRedBeforeEachReopen(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "overflow", err: errors.New("Input overflowed")},
		{name: "device error", err: errors.New("device disconnected")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			capture := newCaptureGate(nil, nil, nil)
			var confirmations int
			capture.beforeMicStart = func() error {
				confirmations++
				if confirmations == 2 {
					cancel()
				}
				return errors.New("recording light stayed off")
			}
			streamer := &fakeStreamer{errs: []error{tc.err}}
			streamMicWithRetry(
				ctx,
				capture,
				streamer,
				bytes.NewBuffer(nil),
				func(time.Duration) {},
				func(string, ...any) {},
				nil,
			)
			if streamer.reopens != 0 {
				t.Fatalf("microphone reopened %d times after red confirmation failed", streamer.reopens)
			}
			if confirmations != 2 {
				t.Fatalf("red confirmations = %d, want 2", confirmations)
			}
			if streamer.stops != 2 {
				t.Fatalf("microphone stops after red confirmation failed = %d, want 2", streamer.stops)
			}
			if streamer.calls != 2 {
				t.Fatalf("stream calls = %d, want first error and stopped retry", streamer.calls)
			}
		})
	}
}

func TestStreamMicWithRetryRetriesDeviceError(t *testing.T) {
	streamer := &fakeStreamer{errs: []error{errors.New("device disconnected"), nil}}
	var waits []time.Duration
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	streamMicWithRetry(ctx, newCaptureGate(nil, nil, nil), streamer, bytes.NewBuffer(nil), func(d time.Duration) {
		waits = append(waits, d)
	}, func(string, ...any) {}, nil)

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
	streamer := &fakeStreamer{
		errs:       []error{errors.New("ENODEV"), errors.New("ENODEV"), errors.New("ENODEV")},
		reopenErrs: []error{errors.New("no device"), errors.New("no device"), errors.New("no device")},
	}
	var waits []time.Duration
	waitCount := make(chan struct{}, 10)
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		for i := 0; i < 3; i++ {
			<-waitCount
		}
		cancel()
	}()

	streamMicWithRetry(ctx, newCaptureGate(nil, nil, nil), streamer, bytes.NewBuffer(nil), func(d time.Duration) {
		waits = append(waits, d)
		waitCount <- struct{}{}
	}, func(string, ...any) {}, nil)

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
	streamer := &fakeStreamer{
		errs:       []error{errors.New("err1"), errors.New("err2"), errors.New("err3"), nil},
		reopenErrs: []error{errors.New("no device"), nil, nil},
	}
	var waits []time.Duration
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	streamMicWithRetry(ctx, newCaptureGate(nil, nil, nil), streamer, bytes.NewBuffer(nil), func(d time.Duration) {
		waits = append(waits, d)
	}, func(string, ...any) {}, nil)

	// err1 → 1s wait, reopen fails → err2 (still in loop) → 2s wait, reopen succeeds (reset) → err3 → 1s wait, reopen succeeds → nil
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

func TestStreamMicWithRetryBroadcastsReconnectAndConnected(t *testing.T) {
	hub := server.NewHub()
	ch := hub.Subscribe()
	defer hub.Unsubscribe(ch)

	streamer := &fakeStreamer{errs: []error{errors.New("device disconnected"), nil}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	streamMicWithRetry(ctx, newCaptureGate(nil, nil, nil), streamer, bytes.NewBuffer(nil), func(time.Duration) {}, func(string, ...any) {}, hub)

	var statuses []string
	deadline := time.After(time.Second)
	for len(statuses) < 3 {
		select {
		case msg := <-ch:
			var payload map[string]any
			if err := json.Unmarshal(msg, &payload); err != nil {
				t.Fatalf("unmarshal failed: %v", err)
			}
			if payload["type"] != "component_status" || payload["component"] != "mic" {
				continue
			}
			status, ok := payload["status"].(string)
			if !ok {
				t.Fatalf("expected string status, got %#v", payload["status"])
			}
			statuses = append(statuses, status)
		case <-deadline:
			t.Fatalf("timeout waiting for mic status events, got %v", statuses)
		}
	}

	want := []string{"error", "reconnecting", "connected"}
	for i, status := range want {
		if statuses[i] != status {
			t.Fatalf("expected statuses %v, got %v", want, statuses)
		}
	}
}

func immediateAfter(time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	ch <- time.Now()
	return ch
}

func oneShotAfter() func(time.Duration) <-chan time.Time {
	fired := false
	return func(time.Duration) <-chan time.Time {
		if fired {
			return make(chan time.Time)
		}
		fired = true
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	}
}

func TestRetryMicStartupRetriesUntilSuccess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var retries int
	var attempts int
	var failures int
	succeeded := false
	errSequence := []error{errors.New("no mic"), nil}

	retryMicStartup(
		ctx,
		time.Second,
		immediateAfter,
		func() { retries++ },
		func() error {
			attempts++
			err := errSequence[0]
			errSequence = errSequence[1:]
			return err
		},
		func(error) { failures++ },
		func() { succeeded = true },
	)

	if retries != 2 {
		t.Fatalf("expected 2 retry notifications, got %d", retries)
	}
	if attempts != 2 {
		t.Fatalf("expected 2 start attempts, got %d", attempts)
	}
	if failures != 1 {
		t.Fatalf("expected 1 failure callback, got %d", failures)
	}
	if !succeeded {
		t.Fatal("expected success callback")
	}
}

func TestRetryMicStartupStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	var attempts int
	var failures int

	retryMicStartup(
		ctx,
		time.Second,
		oneShotAfter(),
		nil,
		func() error {
			attempts++
			return errors.New("still unavailable")
		},
		func(error) {
			failures++
			cancel()
		},
		nil,
	)

	if attempts != 1 {
		t.Fatalf("expected 1 attempt before cancellation, got %d", attempts)
	}
	if failures != 1 {
		t.Fatalf("expected 1 failure callback before cancellation, got %d", failures)
	}
}

// Shutdown that arrives as the retry interval ends (both are ready together, or
// shutdown lands just after the tick) stops the retry before it starts the
// microphone: start re-initializes PortAudio and opens and starts a device,
// which must not happen once shutdown has begun.
func TestRetryMicStartupDoesNotStartTheMicAfterShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := false
	retryMicStartup(
		ctx,
		time.Second,
		oneShotAfter(),
		cancel, // shutdown begins as the retry fires
		func() error {
			started = true
			return nil
		},
		nil,
		nil,
	)
	if started {
		t.Fatal("the microphone was started after shutdown began")
	}
}

func TestStartMicMonitoringUsesRecorderWhenDownstreamMissing(t *testing.T) {
	recorder := audio.NewRecorder(t.TempDir())
	streamer := &fakeStreamer{}

	original := streamMicLoopStarter
	t.Cleanup(func() {
		streamMicLoopStarter = original
	})

	var capturedWriter io.Writer
	var capturedGate *captureGate
	streamMicLoopStarter = func(_ context.Context, _ *sync.WaitGroup, gotCapture *captureGate, gotStreamer micStreamer, writer io.Writer, _ func(time.Duration), _ func(string, ...any), _ *server.Hub) {
		if gotStreamer != streamer {
			t.Fatalf("expected streamer %p, got %p", streamer, gotStreamer)
		}
		capturedGate = gotCapture
		capturedWriter = writer
	}

	capture := newCaptureGate(nil, nil, nil)
	startMicMonitoring(context.Background(), &sync.WaitGroup{}, capture, streamer, recorder, nil, func(time.Duration) {}, func(string, ...any) {}, nil)

	if capturedGate != capture {
		t.Fatal("mic loop did not get the app's capture gate, so pausing would not reach it")
	}
	if capturedWriter == nil {
		t.Fatal("expected monitoring writer to be created")
	}
	if _, err := capturedWriter.Write([]byte{1, 2, 3, 4}); err != nil {
		t.Fatalf("expected writer to accept audio bytes, got %v", err)
	}
}

// Every mic loop runs in the group shutdown waits for, so shutdown can wait
// for it to end before it stops the device and terminates PortAudio.
func TestMicLoopRunsInTheShutdownGroup(t *testing.T) {
	var loops sync.WaitGroup
	mic := newScriptedMic()
	ctx, cancel := context.WithCancel(context.Background())
	startMicMonitoring(ctx, &loops, newCaptureGate(nil, nil, nil), mic, audio.NewRecorder(t.TempDir()), nil, func(time.Duration) {}, func(string, ...any) {}, nil)
	mic.expectEvents(t, "stream")

	if joinMicLoops(&loops, 50*time.Millisecond) {
		t.Error("shutdown would not wait for the running mic loop")
	}
	cancel()
	close(mic.chunks)
	if !joinMicLoops(&loops, testWait) {
		t.Fatal("mic loop not joined after shutdown")
	}
}

// GET /api/status reports the raw recordings kept because they could not be
// saved from a cache refreshed in the background, so a stuck disk or a hung
// recording write cannot hang the status. The warning size is a tenth of the
// audio budget.
func TestRefreshKeptRawRecordings(t *testing.T) {
	dir := t.TempDir()
	keep := func(id string, size int) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, id+".pcm"), make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	keep("20261106100000", 3000)
	var cache atomic.Pointer[server.KeptRawRecordings]
	ctx, cancel := context.WithCancel(context.Background())
	refreshed := goDone(func() {
		refreshKeptRawRecordings(ctx, audio.NewRecorder(dir), func() int { return 10 }, &cache, 10*time.Millisecond)
	})
	t.Cleanup(func() {
		cancel()
		<-refreshed
	})
	waitCached := func(want server.KeptRawRecordings) {
		t.Helper()
		for deadline := time.Now().Add(testWait); ; time.Sleep(time.Millisecond) {
			if got := cache.Load(); got != nil && *got == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("cached kept raw recordings = %+v, want %+v", cache.Load(), want)
			}
		}
	}

	waitCached(server.KeptRawRecordings{Count: 1, Bytes: 3000, WarnBytes: 10 * 1024 * 1024 / 10})
	keep("20261106110000", 1000)
	waitCached(server.KeptRawRecordings{Count: 2, Bytes: 4000, WarnBytes: 10 * 1024 * 1024 / 10})

	cancel()
	if !returnedWithin(refreshed, testWait) {
		t.Fatal("refresh still running after shutdown")
	}
}

// The kept raw recordings warning starts at a tenth of the audio budget. No
// budget wraps it: from 2^43 MB the budget in bytes went negative, so any
// kept recording raised the warning. With no budget (0) it warns at once:
// nothing removes kept recordings.
func TestKeptRawWarningSizeNeverWraps(t *testing.T) {
	if strconv.IntSize < 64 {
		t.Skip("budgets past 2^31 MB need a 64-bit int")
	}
	for _, tc := range []struct {
		budgetMB int64
		want     int64
	}{
		{budgetMB: 10, want: 10 * 1024 * 1024 / 10},
		{budgetMB: 1 << 40, want: (1 << 60) / 10},
		{budgetMB: 1 << 43, want: math.MaxInt64 / 10},
		{budgetMB: math.MaxInt64, want: math.MaxInt64 / 10},
		{budgetMB: 0, want: 0},
	} {
		var cache atomic.Pointer[server.KeptRawRecordings]
		ctx, cancel := context.WithCancel(context.Background())
		refreshed := goDone(func() {
			refreshKeptRawRecordings(ctx, audio.NewRecorder(t.TempDir()), func() int { return int(tc.budgetMB) }, &cache, time.Hour)
		})
		for deadline := time.Now().Add(testWait); cache.Load() == nil; time.Sleep(time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal("kept raw recordings never cached")
			}
		}
		cancel()
		<-refreshed
		if got := cache.Load().WarnBytes; got != tc.want {
			t.Errorf("a %d MB budget warns from %d bytes, want %d", tc.budgetMB, got, tc.want)
		}
	}
}

func TestParseStoredStructuredSummaryWithJSONStringSpeakers(t *testing.T) {
	raw := `{"title":"Nine-hour deadline task triage and QA alignment","summary":"## BLUF\nThe team is urgently prioritizing task unblocking.","speakers":"{\"0\":{\"name\":\"Santa\",\"confidence\":\"mentioned\"}}"}`

	title, summaryText, speakerNames, err := parseStoredStructuredSummary(raw)
	if err != nil {
		t.Fatalf("parseStoredStructuredSummary returned error: %v", err)
	}
	if title != "Nine-hour deadline task triage and QA alignment" {
		t.Fatalf("unexpected title: %q", title)
	}
	if summaryText != "## BLUF\nThe team is urgently prioritizing task unblocking." {
		t.Fatalf("unexpected summary: %q", summaryText)
	}

	var speakers map[string]repairSpeakerMetadata
	if err := json.Unmarshal([]byte(speakerNames), &speakers); err != nil {
		t.Fatalf("speaker names not valid JSON: %v", err)
	}
	if speakers["0"].Name != "Santa" || speakers["0"].Confidence != "mentioned" {
		t.Fatalf("unexpected speakers: %#v", speakers)
	}
}

func TestParseStoredStructuredSummaryWithObjectSpeakers(t *testing.T) {
	raw := `{"title":"Q2 roadmap alignment","summary":"## BLUF\nRoadmap aligned.","speakers":{"1":{"name":"Ben","confidence":"mentioned"}}}`

	title, summaryText, speakerNames, err := parseStoredStructuredSummary(raw)
	if err != nil {
		t.Fatalf("parseStoredStructuredSummary returned error: %v", err)
	}
	if title != "Q2 roadmap alignment" {
		t.Fatalf("unexpected title: %q", title)
	}
	if summaryText != "## BLUF\nRoadmap aligned." {
		t.Fatalf("unexpected summary: %q", summaryText)
	}
	if speakerNames != `{"1":{"name":"Ben","confidence":"mentioned"}}` {
		t.Fatalf("unexpected speakerNames: %s", speakerNames)
	}
}
