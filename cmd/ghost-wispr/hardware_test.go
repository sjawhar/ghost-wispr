package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/indicator"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

type commandLightFake struct {
	colour [3]uint8
	closed bool
}

func (l *commandLightFake) SetRGB(r, g, b uint8) error {
	l.colour = [3]uint8{r, g, b}
	return nil
}
func (l *commandLightFake) ReadRGB() (uint8, uint8, uint8, error) {
	return l.colour[0], l.colour[1], l.colour[2], nil
}
func (*commandLightFake) ServerDown(time.Duration) error { return nil }
func (*commandLightFake) BlackPattern() error            { return nil }
func (l *commandLightFake) Close() error {
	l.closed = true
	return nil
}

// TestLightOffSubcommandSendsOff fails if systemd's stop hook leaves the
// recording indicator lit.
func TestLightOffSubcommandSendsOff(t *testing.T) {
	light := &commandLightFake{colour: [3]uint8{255, 0, 0}}
	original := openBlink1
	openBlink1 = func(string, string) (indicator.Light, error) { return light, nil }
	t.Cleanup(func() { openBlink1 = original })

	var stderr bytes.Buffer
	if err := runLightOff(&stderr); err != nil {
		t.Fatalf("light-off: %v", err)
	}
	if light.colour != [3]uint8{} {
		t.Fatalf("light after light-off = %v, want off", light.colour)
	}
	if !light.closed {
		t.Fatal("light-off did not close the light")
	}
}

func TestLightOffSubcommandWithoutDeviceSucceeds(t *testing.T) {
	original := openBlink1
	openBlink1 = func(string, string) (indicator.Light, error) { return nil, indicator.ErrNoBlink1 }
	t.Cleanup(func() { openBlink1 = original })

	var stderr bytes.Buffer
	if err := runLightOff(&stderr); err != nil {
		t.Fatalf("light-off without light: %v", err)
	}
	if !strings.Contains(stderr.String(), "not found") {
		t.Fatalf("light-off without light stderr = %q, want reason", stderr.String())
	}
}

func TestLightStateSubcommand(t *testing.T) {
	for _, tc := range []struct {
		name    string
		color   [3]uint8
		want    string
		wantErr bool
	}{
		{name: "red", color: [3]uint8{255, 0, 0}, want: "red\n"},
		{name: "off", color: [3]uint8{}, want: "off\n"},
		{name: "other", color: [3]uint8{0, 255, 0}, want: "other 0,255,0\n", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			light := &commandLightFake{colour: tc.color}
			original := openBlink1
			openBlink1 = func(string, string) (indicator.Light, error) { return light, nil }
			t.Cleanup(func() { openBlink1 = original })
			var stdout bytes.Buffer
			err := runLightState(&stdout)
			if (err != nil) != tc.wantErr {
				t.Fatalf("light-state error = %v, want error=%t", err, tc.wantErr)
			}
			if got := stdout.String(); got != tc.want {
				t.Fatalf("light-state output = %q, want %q", got, tc.want)
			}
		})
	}
}

type openMicFake struct{ open bool }

func (m *openMicFake) IsOpen() bool { return m.open }

// TestLightFollowsTheGateAndTheMic fails if the indicator is red while
// capture is paused or the microphone is not actually capturing.
func TestLightFollowsTheGateAndTheMic(t *testing.T) {
	capture := newCaptureGate(nil, nil, nil)
	mic := &openMicFake{}
	if recordingSignal(capture, mic) {
		t.Fatal("closed microphone was reported as recording")
	}
	mic.open = true
	if !recordingSignal(capture, mic) {
		t.Fatal("open microphone and open gate were not reported as recording")
	}
	capture.Pause()
	if recordingSignal(capture, mic) {
		t.Fatal("paused capture was reported as recording")
	}
}

// TestTapTogglesAndPersists fails if hardware control bypasses the same
// durable controls used by the HTTP pause and resume routes.
func TestTapTogglesAndPersists(t *testing.T) {
	store, err := storage.NewSQLiteStore(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	capture := newCaptureGate(nil, nil, nil)
	pause, resume := persistingControls(store, capture.Pause, capture.Resume)
	var statuses []bool
	if err := toggleCapture(capture.IsPaused, pause, resume, func(paused bool) { statuses = append(statuses, paused) }); err != nil {
		t.Fatalf("pause tap: %v", err)
	}
	value, ok, err := store.GetAppState(storage.AppStateCapturePaused)
	if err != nil || !ok || value != "1" || !capture.IsPaused() {
		t.Fatalf("pause tap state = (%q, %t, %v, paused=%t), want (1, true, nil, true)", value, ok, err, capture.IsPaused())
	}
	if err := toggleCapture(capture.IsPaused, pause, resume, func(paused bool) { statuses = append(statuses, paused) }); err != nil {
		t.Fatalf("resume tap: %v", err)
	}
	value, ok, err = store.GetAppState(storage.AppStateCapturePaused)
	if err != nil || !ok || value != "0" || capture.IsPaused() {
		t.Fatalf("resume tap state = (%q, %t, %v, paused=%t), want (0, true, nil, false)", value, ok, err, capture.IsPaused())
	}
	if got, want := statuses, []bool{true, false}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("status events = %v, want %v", got, want)
	}
}

func TestTapFailureBroadcastsPause(t *testing.T) {
	var statuses []bool
	err := toggleCapture(
		func() bool { return false },
		func() error { return errors.New("state store unavailable") },
		func() error { return nil },
		func(paused bool) { statuses = append(statuses, paused) },
	)
	if err == nil {
		t.Fatal("pause toggle succeeded after state failure")
	}
	if got, want := statuses, []bool{true}; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("status events = %v, want %v", got, want)
	}
}

type reconnectLightFake struct {
	mu     sync.Mutex
	colour [3]uint8
	setErr error
	closed bool
}

func (l *reconnectLightFake) SetRGB(r, g, b uint8) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.setErr != nil {
		return l.setErr
	}
	l.colour = [3]uint8{r, g, b}
	return nil
}

func (l *reconnectLightFake) ReadRGB() (uint8, uint8, uint8, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.colour[0], l.colour[1], l.colour[2], nil
}

func (*reconnectLightFake) ServerDown(time.Duration) error { return nil }
func (*reconnectLightFake) BlackPattern() error            { return nil }

func (l *reconnectLightFake) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	return nil
}

func (l *reconnectLightFake) isRed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.colour == [3]uint8{255, 0, 0}
}

func (l *reconnectLightFake) isClosed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closed
}

// TestControllerReopensAfterAWriteError fails if a transient USB failure leaves
// the recorder running without retrying the indicator device.
func TestControllerReopensAfterAWriteError(t *testing.T) {
	first := &reconnectLightFake{setErr: errors.New("device disconnected")}
	second := &reconnectLightFake{}
	original := openBlink1
	var opens int
	openBlink1 = func(string, string) (indicator.Light, error) {
		opens++
		if opens == 1 {
			return first, nil
		}
		return second, nil
	}
	t.Cleanup(func() { openBlink1 = original })

	warnings := newWarningsRegistry()
	controller := indicator.NewController(&reconnectingLight{warnings: warnings}, func() bool { return true }, time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { controller.Run(ctx); close(done) }()

	deadline := time.Now().Add(time.Second)
	for !second.isRed() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !second.isRed() {
		t.Fatal("replacement blink(1) was not set red after the write error")
	}
	if !first.isClosed() {
		t.Fatal("failed blink(1) device was not closed")
	}
	if got := warnings.List(); len(got) != 0 {
		t.Fatalf("warnings after replacement opened = %v, want none", got)
	}
	cancel()
	<-done
}

// TestDefaultListenAddressIsLoopback fails if an unset address exposes the
// recorder without Tailscale Serve in front of it.
func TestDefaultListenAddressIsLoopback(t *testing.T) {
	if got := listenAddress(""); got != "127.0.0.1:8080" {
		t.Fatalf("default listen address = %q, want 127.0.0.1:8080", got)
	}
	if got := listenAddress(":8081"); got != ":8081" {
		t.Fatalf("explicit listen address = %q, want :8081", got)
	}
}

func TestReconnectingLightRetainsAccessFailureDuringRetry(t *testing.T) {
	accessErr := errors.New("permission denied")
	original := openBlink1
	openBlink1 = func(string, string) (indicator.Light, error) { return nil, accessErr }
	t.Cleanup(func() { openBlink1 = original })

	light := &reconnectingLight{warnings: newWarningsRegistry()}
	if _, err := light.current(); !errors.Is(err, accessErr) {
		t.Fatalf("first open error = %v, want %v", err, accessErr)
	}
	if _, err := light.current(); !errors.Is(err, accessErr) {
		t.Fatalf("retry-window error = %v, want %v", err, accessErr)
	}
}
