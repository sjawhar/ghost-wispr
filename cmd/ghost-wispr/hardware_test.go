package main

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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

type openMicFake struct{ open atomic.Bool }

func (m *openMicFake) IsOpen() bool { return m.open.Load() }

// TestLightFollowsTheGateAndTheMic fails if the indicator is red while
// capture is paused or the microphone is not actually capturing.
func TestLightFollowsTheGateAndTheMic(t *testing.T) {
	capture := newCaptureGate(nil, nil, nil)
	mic := &openMicFake{}
	if recordingSignal(capture, mic) {
		t.Fatal("closed microphone was reported as recording")
	}
	mic.open.Store(true)
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
	controls := newPersistentControls(store, capture.Pause, capture.RequestResume, nil)
	pause, resume := controls.Pause, controls.Resume
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

type operationLightFake struct {
	mu         sync.Mutex
	colour     [3]uint8
	offStarted chan struct{}
	releaseOff chan struct{}
	offOnce    sync.Once
	reads      [][3]uint8
}

func (l *operationLightFake) SetRGB(r, g, b uint8) error {
	if r == 0 && g == 0 && b == 0 && l.offStarted != nil {
		l.offOnce.Do(func() {
			close(l.offStarted)
			<-l.releaseOff
		})
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.colour = [3]uint8{r, g, b}
	return nil
}

func (l *operationLightFake) ReadRGB() (uint8, uint8, uint8, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reads = append(l.reads, l.colour)
	return l.colour[0], l.colour[1], l.colour[2], nil
}

func (*operationLightFake) ServerDown(time.Duration) error { return nil }
func (*operationLightFake) BlackPattern() error            { return nil }
func (*operationLightFake) Close() error                   { return nil }

func (l *operationLightFake) snapshot() ([3]uint8, [][3]uint8) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.colour, append([][3]uint8(nil), l.reads...)
}

func waitForHardware(t *testing.T, description string, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

// TestEnsureRedSerializesControllerUpdate fails if the controller can turn the
// light off after a successful red confirmation.
func TestEnsureRedSerializesControllerUpdate(t *testing.T) {
	device := &operationLightFake{
		colour:     [3]uint8{255, 0, 0},
		offStarted: make(chan struct{}),
		releaseOff: make(chan struct{}),
	}
	release := sync.OnceFunc(func() { close(device.releaseOff) })
	hardware := &hardwareControl{
		light: &reconnectingLight{light: device, warnings: newWarningsRegistry()},
	}
	hardware.controller = indicator.NewController(hardware.light, hardware.holdsRed, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		hardware.controller.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		release()
		cancel()
		<-done
	})

	select {
	case <-device.offStarted:
	case <-time.After(time.Second):
		t.Fatal("controller did not begin its off update")
	}
	confirmed := make(chan error, 1)
	go func() { confirmed <- hardware.EnsureRed() }()
	select {
	case err := <-confirmed:
		t.Fatalf("EnsureRed returned before the controller update completed: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	release()
	if err := <-confirmed; err != nil {
		t.Fatalf("EnsureRed: %v", err)
	}
	colour, reads := device.snapshot()
	if colour != [3]uint8{255, 0, 0} {
		t.Fatalf("colour after confirmed red = %v, want red", colour)
	}
	if got, want := len(reads), 2; got != want || reads[0] != [3]uint8{255, 0, 0} || reads[1] != [3]uint8{255, 0, 0} {
		t.Fatalf("read replies = %v, want each operation's red reply", reads)
	}
}

type closeRaceLight struct {
	setStarted chan struct{}
	releaseSet chan struct{}
	closeCalls atomic.Int32
}

func (l *closeRaceLight) SetRGB(uint8, uint8, uint8) error {
	close(l.setStarted)
	<-l.releaseSet
	return nil
}
func (*closeRaceLight) ReadRGB() (uint8, uint8, uint8, error) { return 0, 0, 0, nil }
func (*closeRaceLight) ServerDown(time.Duration) error        { return nil }
func (*closeRaceLight) BlackPattern() error                   { return nil }
func (l *closeRaceLight) Close() error {
	l.closeCalls.Add(1)
	return nil
}

// TestReconnectingLightDoesNotCloseDuringAnOperation fails if Close can
// invalidate a feature report still in flight.
func TestReconnectingLightDoesNotCloseDuringAnOperation(t *testing.T) {
	device := &closeRaceLight{setStarted: make(chan struct{}), releaseSet: make(chan struct{})}
	release := sync.OnceFunc(func() { close(device.releaseSet) })
	t.Cleanup(release)
	light := &reconnectingLight{light: device, warnings: newWarningsRegistry()}
	setDone := make(chan error, 1)
	go func() { setDone <- light.SetRGB(255, 0, 0) }()
	select {
	case <-device.setStarted:
	case <-time.After(time.Second):
		t.Fatal("feature report did not start")
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- light.Close() }()
	select {
	case err := <-closeDone:
		t.Fatalf("Close returned before the in-flight feature report completed: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	release()
	if err := <-setDone; err != nil {
		t.Fatalf("set red: %v", err)
	}
	if err := <-closeDone; err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := device.closeCalls.Load(); got != 1 {
		t.Fatalf("close calls = %d, want 1", got)
	}
}

type staleReadLight struct{}

func (*staleReadLight) SetRGB(uint8, uint8, uint8) error { return nil }
func (*staleReadLight) ReadRGB() (uint8, uint8, uint8, error) {
	return 0, 0, 0, nil
}
func (*staleReadLight) ServerDown(time.Duration) error { return nil }
func (*staleReadLight) BlackPattern() error            { return nil }
func (*staleReadLight) Close() error                   { return nil }

// TestEnsureRedRequiresReadback fails if a write is trusted without confirming
// that the light is actually red.
func TestEnsureRedRequiresReadback(t *testing.T) {
	hardware := &hardwareControl{
		light: &reconnectingLight{light: &staleReadLight{}, warnings: newWarningsRegistry()},
	}
	if err := hardware.EnsureRed(); err == nil {
		t.Fatal("EnsureRed succeeded with an off readback")
	}
	if hardware.holdsRed() {
		t.Fatal("EnsureRed held the light red after a failed readback")
	}
}

// TestWireLightConsentRequiresRedBeforeStartOrResume fails if recorder wiring
// bypasses the configured indicator's red confirmation.
func TestWireLightConsentRequiresRedBeforeStartOrResume(t *testing.T) {
	store, err := storage.NewSQLiteStore(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	hardware := &hardwareControl{
		light: &reconnectingLight{light: &staleReadLight{}, warnings: newWarningsRegistry()},
	}
	capture := newCaptureGate(nil, nil, nil)
	wireLightConsent(capture, hardware, newWarningsRegistry(), nil)
	started := false
	if err := startMicIfUnpaused(capture, func() error {
		started = true
		return nil
	}); err == nil {
		t.Fatal("microphone started after red confirmation failed")
	}
	if started {
		t.Fatal("microphone start ran after red confirmation failed")
	}
	capture.Pause()
	capture.Resume()
	waitForHardware(t, "failed red confirmation to keep wired resume paused", capture.IsPaused)
}

// TestConfirmRedRetriesDiscoveryBeforeRecording fails if a cached missing-light
// result lets capture begin after the blink(1) appears.
func TestConfirmRedRetriesDiscoveryBeforeRecording(t *testing.T) {
	device := &operationLightFake{}
	original := openBlink1
	var opens int
	openBlink1 = func(string, string) (indicator.Light, error) {
		opens++
		if opens == 1 {
			return nil, indicator.ErrNoBlink1
		}
		return device, nil
	}
	t.Cleanup(func() { openBlink1 = original })
	light := &reconnectingLight{warnings: newWarningsRegistry()}
	if _, err := light.current(); !errors.Is(err, indicator.ErrNoBlink1) {
		t.Fatalf("initial discovery error = %v, want ErrNoBlink1", err)
	}
	if err := light.ConfirmRed(); err != nil {
		t.Fatalf("confirm red after light appears: %v", err)
	}
	if opens != 2 {
		t.Fatalf("blink(1) discoveries = %d, want 2", opens)
	}
	if colour, _ := device.snapshot(); colour != [3]uint8{255, 0, 0} {
		t.Fatalf("colour after rediscovery = %v, want red", colour)
	}
}

// TestMicStatusEventKeepsRedHold fails if a delayed reconnect event can clear
// the confirmation that protects another start attempt.
func TestMicStatusEventKeepsRedHold(t *testing.T) {
	hardware := &hardwareControl{}
	hardware.redHolds.Store(1)
	wakeLightOnCaptureEvent([]byte(`{"type":"component_status","component":"mic"}`), hardware)
	if !hardware.holdsRed() {
		t.Fatal("mic status event cleared a pending red hold")
	}
}

// TestResumeRequiresRedBeforeOpeningCapture fails if a resume can open capture
// after the light refuses its red confirmation.
func TestResumeRequiresRedBeforeOpeningCapture(t *testing.T) {
	store, err := storage.NewSQLiteStore(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	capture := newCaptureGate(nil, nil, nil)
	var beforeResume atomic.Bool
	capture.beforeResume = func() { beforeResume.Store(true) }
	var confirmations atomic.Int32
	capture.beforeMicStart = func() error {
		confirmations.Add(1)
		return errors.New("light is off")
	}
	guardResumeWithLight(capture, newWarningsRegistry(), nil)
	controls := newPersistentControls(store, capture.Pause, capture.RequestResume, nil)
	capture.Pause()
	if err := controls.Resume(); err == nil {
		t.Fatal("resume succeeded after red confirmation failed")
	}
	if confirmations.Load() != 1 || !capture.IsPaused() {
		t.Fatalf("confirmation calls=%d paused=%t, want (1, true)", confirmations.Load(), capture.IsPaused())
	}
	if !capture.IsPaused() {
		t.Fatal("capture resumed after red confirmation failed")
	}
	if beforeResume.Load() {
		t.Fatal("capture's normal resume work ran after red confirmation failed")
	}
	value, ok, err := store.GetAppState(storage.AppStateCapturePaused)
	if err != nil || !ok || value != "1" {
		t.Fatalf("persisted pause after red confirmation failure = (%q, %t, %v), want (1, true, nil)", value, ok, err)
	}
}

// TestResumeClearsForcedRedWhenMicUnavailable fails if resuming with no
// microphone leaves the indicator red while no audio can flow.
func TestResumeClearsForcedRedWhenMicUnavailable(t *testing.T) {
	device := &operationLightFake{}
	hardware := &hardwareControl{
		light: &reconnectingLight{light: device, warnings: newWarningsRegistry()},
	}
	hardware.light.holdRed = hardware.holdsRed
	hardware.controller = indicator.NewController(hardware.light, hardware.holdsRed, time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		hardware.controller.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	store, err := storage.NewSQLiteStore(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	capture := newCaptureGate(nil, nil, nil)
	clearStarted := make(chan struct{})
	releaseClear := make(chan struct{})
	release := sync.OnceFunc(func() { close(releaseClear) })
	t.Cleanup(release)
	capture.beforeMicStart = hardware.EnsureRed
	capture.afterMicStart = hardware.ReleaseRedHold
	capture.afterGateOpen = func() {
		close(clearStarted)
		<-releaseClear
		hardware.ReleaseRedHold()
	}
	guardResumeWithLight(capture, newWarningsRegistry(), nil)
	capture.Pause()
	capture.Resume()
	select {
	case <-clearStarted:
	case <-time.After(time.Second):
		t.Fatal("resume did not clear forced red without a microphone")
	}
	colour, _ := device.snapshot()
	if colour != [3]uint8{255, 0, 0} || !hardware.holdsRed() {
		t.Fatalf("light before clearing forced red = (%v, forced=%t), want (red, true)", colour, hardware.holdsRed())
	}
	release()
	waitForHardware(t, "light to turn off without a microphone", func() bool {
		colour, _ := device.snapshot()
		return !hardware.holdsRed() && colour == [3]uint8{}
	})
	if hardware.holdsRed() {
		t.Fatal("forced red remained set without a microphone")
	}
}

// TestEnsureRedOutlastsAStaleOffDecision fails if the controller, having
// decided "off" before a red confirmation, can still turn the light off after
// EnsureRed has returned.
func TestEnsureRedOutlastsAStaleOffDecision(t *testing.T) {
	device := &operationLightFake{
		colour:     [3]uint8{255, 0, 0},
		offStarted: make(chan struct{}),
		releaseOff: make(chan struct{}),
	}
	releaseOff := sync.OnceFunc(func() { close(device.releaseOff) })
	hardware := &hardwareControl{
		light: &reconnectingLight{light: device, warnings: newWarningsRegistry()},
	}
	hardware.light.holdRed = hardware.holdsRed
	decided := make(chan struct{})
	releaseDecision := make(chan struct{})
	release := sync.OnceFunc(func() { close(releaseDecision) })
	var decisions atomic.Int32
	hardware.controller = indicator.NewController(hardware.light, func() bool {
		want := hardware.holdsRed()
		if decisions.Add(1) == 1 {
			close(decided)
			<-releaseDecision
		}
		return want
	}, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		hardware.controller.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		release()
		releaseOff()
		cancel()
		<-done
	})

	select {
	case <-decided:
	case <-time.After(time.Second):
		t.Fatal("controller did not decide its first update")
	}
	if err := hardware.EnsureRed(); err != nil {
		t.Fatalf("EnsureRed: %v", err)
	}
	release()
	waitForHardware(t, "controller read after its stale decision", func() bool {
		_, reads := device.snapshot()
		return len(reads) >= 2
	})
	select {
	case <-device.offStarted:
		t.Fatal("controller wrote off after EnsureRed returned")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestResumeReleasesRedHoldAfterGateOpens fails if a short pause leaves the
// confirmation hold set after the microphone resumes without restarting.
func TestResumeReleasesRedHoldAfterGateOpens(t *testing.T) {
	device := &operationLightFake{}
	capture := newCaptureGate(nil, nil, nil)
	capture.Pause()
	mic := &openMicFake{}
	mic.open.Store(true)
	hardware := &hardwareControl{
		light: &reconnectingLight{light: device, warnings: newWarningsRegistry()},
	}
	hardware.light.holdRed = hardware.holdsRed
	hardware.controller = indicator.NewController(hardware.light, func() bool {
		return hardware.holdsRed() || recordingSignal(capture, mic)
	}, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		hardware.controller.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	store, err := storage.NewSQLiteStore(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	wireLightConsent(capture, hardware, newWarningsRegistry(), nil)
	capture.Resume()
	waitForHardware(t, "gate-open red hold release", func() bool {
		colour, _ := device.snapshot()
		return !hardware.holdsRed() && colour == [3]uint8{255, 0, 0}
	})
	mic.open.Store(false)
	hardware.Wake()
	waitForHardware(t, "light off after microphone loss", func() bool {
		colour, _ := device.snapshot()
		return !hardware.holdsRed() && colour == [3]uint8{}
	})
}

// TestResumeReleasesOnlyItsOwnRedHold fails if opening the gate consumes a
// second start attempt's red hold.
func TestResumeReleasesOnlyItsOwnRedHold(t *testing.T) {
	device := &operationLightFake{}
	hardware := &hardwareControl{
		light: &reconnectingLight{light: device, warnings: newWarningsRegistry()},
	}
	capture := newCaptureGate(nil, nil, nil)
	capture.Pause()
	hardware.redHolds.Store(1)
	store, err := storage.NewSQLiteStore(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	wireLightConsent(capture, hardware, newWarningsRegistry(), nil)
	released := make(chan struct{})
	afterGateOpen := capture.afterGateOpen
	capture.afterGateOpen = func() {
		time.Sleep(50 * time.Millisecond)
		afterGateOpen()
		close(released)
	}
	capture.Resume()
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for gate-open red hold release")
	}
	colour, _ := device.snapshot()
	if colour != [3]uint8{255, 0, 0} {
		t.Fatalf("light after resume = %v, want red", colour)
	}
	if got := hardware.redHolds.Load(); got != 1 {
		t.Fatalf("red holds after resume = %d, want 1", got)
	}
}

// TestReconnectingLightWarnsWhenPresentDeviceFails fails if a present light is
// described as missing after its own write fails.
func TestReconnectingLightWarnsWhenPresentDeviceFails(t *testing.T) {
	warnings := newWarningsRegistry()
	light := &reconnectingLight{
		light:    &reconnectLightFake{setErr: errors.New("USB write failed")},
		warnings: warnings,
	}
	if err := light.SetRGB(255, 0, 0); err == nil {
		t.Fatal("set red succeeded despite the device error")
	}
	if got, want := warnings.List(), []string{"Recording light failed; the unit may be recording without its light"}; !slices.Equal(got, want) {
		t.Fatalf("warnings = %v, want %v", got, want)
	}
}

// TestUnknownLightCommandPrintsUsage fails if an unrecognised command has an
// ambiguous success-looking exit code or omits its usage.
func TestUnknownLightCommandPrintsUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if got := runLightCommand([]string{"unexpected"}, &stdout, &stderr); got != 2 {
		t.Fatalf("unknown command exit code = %d, want 2", got)
	}
	if stdout.Len() != 0 {
		t.Fatalf("unknown command wrote stdout = %q, want empty", stdout.String())
	}
	if got := stderr.String(); !strings.Contains(got, `unknown subcommand "unexpected"`) || !strings.Contains(got, "usage: ghost-wispr") {
		t.Fatalf("unknown command stderr = %q, want reason and usage", got)
	}
}

// TestLightCommandRejectsExtraArgument fails if a light command ignores an
// extra argument instead of printing usage with exit code 2.
func TestLightCommandRejectsExtraArgument(t *testing.T) {
	for _, args := range [][]string{{"light-off", "extra"}, {"light-state", "extra"}} {
		var stdout, stderr bytes.Buffer
		if got := runLightCommand(args, &stdout, &stderr); got != 2 {
			t.Fatalf("%v exit code = %d, want 2", args, got)
		}
		if !strings.Contains(stderr.String(), "usage: ghost-wispr") {
			t.Fatalf("%v stderr = %q, want usage", args, stderr.String())
		}
	}
}

// TestResumeDoesNotLightBeforeGateOpens fails if the request-time confirmation
// leaves its red hold set while a paused session is still settling.
func TestResumeDoesNotLightBeforeGateOpens(t *testing.T) {
	settled := make(chan struct{})
	capture := newCaptureGate(nil, func() { <-settled }, nil)
	device := &operationLightFake{}
	mic := &openMicFake{}
	hardware := &hardwareControl{
		light: &reconnectingLight{light: device, warnings: newWarningsRegistry()},
	}
	hardware.light.holdRed = hardware.holdsRed
	hardware.controller = indicator.NewController(hardware.light, func() bool {
		return hardware.holdsRed() || recordingSignal(capture, mic)
	}, time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		hardware.controller.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		close(settled)
		cancel()
		<-done
	})

	store, err := storage.NewSQLiteStore(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	controls := newPersistentControls(store, capture.Pause, capture.RequestResume, nil)
	wireLightConsent(capture, hardware, newWarningsRegistry(), nil)
	capture.Pause()
	if err := controls.Resume(); err != nil {
		t.Fatalf("resume request: %v", err)
	}
	waitForHardware(t, "light off while resume settles", func() bool {
		colour, _ := device.snapshot()
		return !hardware.holdsRed() && colour == [3]uint8{}
	})
}

type redThenOffLight struct{ reads int }

func (*redThenOffLight) SetRGB(uint8, uint8, uint8) error { return nil }
func (l *redThenOffLight) ReadRGB() (uint8, uint8, uint8, error) {
	l.reads++
	if l.reads == 1 {
		return 255, 0, 0, nil
	}
	return 0, 0, 0, nil
}
func (*redThenOffLight) ServerDown(time.Duration) error { return nil }
func (*redThenOffLight) BlackPattern() error            { return nil }
func (*redThenOffLight) Close() error                   { return nil }

// TestResumeKeepsGateClosedWhenLightFailsDuringSettle fails if a later light
// failure opens the gate onto an active microphone.
func TestResumeKeepsGateClosedWhenLightFailsDuringSettle(t *testing.T) {
	settled := make(chan struct{})
	capture := newCaptureGate(nil, func() { <-settled }, nil)
	mic := &openMicFake{}
	mic.open.Store(true)
	store, err := storage.NewSQLiteStore(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	hardware := &hardwareControl{
		light: &reconnectingLight{light: &redThenOffLight{}, warnings: newWarningsRegistry()},
	}
	var (
		statuses   []bool
		statusesMu sync.Mutex
	)
	controls := newPersistentControls(store, capture.Pause, capture.RequestResume, func(paused bool) {
		statusesMu.Lock()
		defer statusesMu.Unlock()
		statuses = append(statuses, paused)
	})
	wireLightConsent(capture, hardware, newWarningsRegistry(), func(error) {
		if err := controls.RefuseResume(); err != nil {
			t.Errorf("persist refused resume: %v", err)
		}
	})
	capture.Pause()
	if err := controls.Resume(); err != nil {
		t.Fatalf("resume request: %v", err)
	}
	close(settled)
	waitForHardware(t, "light failure to re-pause capture", func() bool {
		return capture.IsPaused() && capture.isClosed() && !recordingSignal(capture, mic)
	})
	value, ok, err := store.GetAppState(storage.AppStateCapturePaused)
	if err != nil || !ok || value != "1" {
		t.Fatalf("persisted pause after gate-open refusal = (%q, %t, %v), want (1, true, nil)", value, ok, err)
	}
	statusesMu.Lock()
	defer statusesMu.Unlock()
	if got, want := statuses, []bool{false, true}; !slices.Equal(got, want) {
		t.Fatalf("status broadcasts = %v, want %v", got, want)
	}
}
