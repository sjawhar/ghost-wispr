package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/indicator"
	"github.com/sjawhar/ghost-wispr/internal/server"
)

// pluggableLight is a blink(1) that can be unplugged and plugged back in.
// Unplugged, each feature report fails and discovery finds no device. With
// readsOff it takes every colour but always reads back off, as a light whose
// LED has failed.
type pluggableLight struct {
	mu       sync.Mutex
	present  bool
	readsOff bool
	colour   [3]uint8
}

var errLightUnplugged = errors.New("blink(1) feature report: no such device")

func (l *pluggableLight) SetRGB(r, g, b uint8) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.present {
		return errLightUnplugged
	}
	l.colour = [3]uint8{r, g, b}
	return nil
}

func (l *pluggableLight) ReadRGB() (uint8, uint8, uint8, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.present {
		return 0, 0, 0, errLightUnplugged
	}
	if l.readsOff {
		return 0, 0, 0, nil
	}
	return l.colour[0], l.colour[1], l.colour[2], nil
}

func (l *pluggableLight) ServerDown(time.Duration) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.present {
		return errLightUnplugged
	}
	return nil
}

func (l *pluggableLight) BlackPattern() error { return l.ServerDown(0) }
func (*pluggableLight) Close() error          { return nil }

func (l *pluggableLight) plug(present bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.present = present
	if !present {
		l.colour = [3]uint8{}
	}
}

func (l *pluggableLight) setReadsOff(readsOff bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.readsOff = readsOff
}

func (l *pluggableLight) isRed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.present && !l.readsOff && l.colour == [3]uint8{255, 0, 0}
}

// startTestLight starts the recording light as the recorder does, on device,
// checking it every checkEvery and retrying a lost device every few
// milliseconds.
func startTestLight(t *testing.T, device *pluggableLight, checkEvery time.Duration, capture *captureGate, mic interface{ IsOpen() bool }, warnings *warningsRegistry) *hardwareControl {
	t.Helper()
	originalOpen, originalCheck, originalRetry := openBlink1, lightCheckEvery, lightRetryEvery
	openBlink1 = func(string, string) (indicator.Light, error) {
		device.mu.Lock()
		defer device.mu.Unlock()
		if !device.present {
			return nil, indicator.ErrNoBlink1
		}
		return device, nil
	}
	lightCheckEvery = checkEvery
	lightRetryEvery = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	hardware := startHardware(ctx, &config.Config{IndicatorLight: config.IndicatorLightBlink1}, capture, mic, warnings, server.NewHub())
	t.Cleanup(func() {
		// Stop first, so a reopening still settling cannot reach openBlink1
		// once the controller has closed the device.
		hardware.Stop()
		cancel()
		<-hardware.Done()
		openBlink1, lightCheckEvery, lightRetryEvery = originalOpen, originalCheck, originalRetry
	})
	wireLightConsent(capture, hardware, warnings, func(err error) {
		t.Errorf("an automatic light check refused a resume as a person's: %v", err)
	})
	return hardware
}

func warningAbout(warnings *warningsRegistry, words ...string) bool {
	return slices.ContainsFunc(warnings.List(), func(warning string) bool {
		for _, word := range words {
			if !strings.Contains(warning, word) {
				return false
			}
		}
		return true
	})
}

// TestNoLightStartsNoMicrophone fails if the recorder starts its recording
// microphone with indicator_light: blink1 and no blink(1) plugged in. The
// paused listener alone may hold the microphone with the light off, in
// listening, and its phrase still opens no recording microphone.
func TestNoLightStartsNoMicrophone(t *testing.T) {
	t.Run("the recording microphone", func(t *testing.T) {
		device := &pluggableLight{}
		capture := newCaptureGate(nil, nil, nil)
		mic := &openMicFake{}
		warnings := newWarningsRegistry()
		startTestLight(t, device, time.Hour, capture, mic, warnings)

		started := false
		if err := startMicIfUnpaused(capture, func() error {
			started = true
			return nil
		}); err == nil {
			t.Error("microphone start reported success with no recording light")
		}
		if started {
			t.Fatal("microphone started with no recording light")
		}
		waitForHardware(t, "a warning that the light is missing and the unit does not record", func() bool {
			return warningAbout(warnings, "Recording light not found", "does not record")
		})
		waitForHardware(t, "capture to be held for the light", func() bool {
			return capture.isClosed() && !capture.IsPaused() && capture.captureState(mic) == "light_unavailable"
		})
	})
	t.Run("the paused listener alone, in listening", requireTheListenerAloneWithNoLight)
}

// TestLightFailureWhileCapturingStopsCapture fails if capture goes on after
// the blink(1) is unplugged mid-recording, or if one controller check does not
// close the gate.
func TestLightFailureWhileCapturingStopsCapture(t *testing.T) {
	device := &pluggableLight{present: true}
	capture := newCaptureGate(nil, nil, nil)
	mic := &openMicFake{}
	mic.open.Store(true)
	warnings := newWarningsRegistry()
	hardware := startTestLight(t, device, time.Hour, capture, mic, warnings)
	waitForHardware(t, "the light red while capturing", device.isRed)

	device.plug(false)
	hardware.Wake() // one controller check
	waitForHardware(t, "capture to stop after one check of the unplugged light", func() bool {
		return capture.isClosed() && !recordingSignal(capture, mic)
	})
	if capture.IsPaused() {
		t.Fatal("the light failure moved the pause switch; only a person pauses")
	}
	if got := capture.captureState(mic); got != "light_unavailable" {
		t.Fatalf("capture state with the light unplugged = %q, want light_unavailable", got)
	}
	if !warningAbout(warnings, "Recording light", "does not record") {
		t.Fatalf("warnings with the light unplugged = %v, want one naming the light and saying the unit does not record", warnings.List())
	}
}

// TestCaptureResumesWhenTheLightIsRedAgain fails if capture stays stopped
// after the blink(1) is plugged back in and confirmed red.
func TestCaptureResumesWhenTheLightIsRedAgain(t *testing.T) {
	device := &pluggableLight{present: true}
	capture := newCaptureGate(nil, nil, nil)
	mic := &openMicFake{}
	mic.open.Store(true)
	warnings := newWarningsRegistry()
	startTestLight(t, device, time.Millisecond, capture, mic, warnings)
	waitForHardware(t, "the light red while capturing", device.isRed)
	device.plug(false)
	waitForHardware(t, "capture to stop while the light is unplugged", capture.isClosed)

	device.plug(true)
	waitForHardware(t, "capture to resume with the light red", func() bool {
		return recordingSignal(capture, mic) && device.isRed()
	})
	if capture.IsPaused() {
		t.Fatal("capture resumed with the pause switch off")
	}
	if got := warnings.List(); len(got) != 0 {
		t.Fatalf("warnings after the light is red again = %v, want none", got)
	}
}

// TestLightReturnKeepsAPersonsPause fails if the light coming back resumes a
// unit a person paused while the light was missing.
func TestLightReturnKeepsAPersonsPause(t *testing.T) {
	device := &pluggableLight{present: true}
	capture := newCaptureGate(nil, nil, nil)
	mic := &openMicFake{}
	mic.open.Store(true)
	warnings := newWarningsRegistry()
	startTestLight(t, device, time.Millisecond, capture, mic, warnings)
	waitForHardware(t, "the light red while capturing", device.isRed)
	device.plug(false)
	waitForHardware(t, "capture to stop while the light is unplugged", capture.isClosed)
	mic.open.Store(false) // the mic loop closes the microphone once the gate closes

	capture.Pause()
	device.plug(true)
	time.Sleep(50 * time.Millisecond)
	if !capture.IsPaused() || !capture.isClosed() || device.isRed() || capture.isLightHeld() {
		t.Fatalf("after the light came back: paused=%t closed=%t red=%t light hold=%t, want paused, closed, dark and the light hold released",
			capture.IsPaused(), capture.isClosed(), device.isRed(), capture.isLightHeld())
	}
}

// TestLightRefusedStartBlamesTheLight fails if a microphone start the light
// refused adds a warning that blames the microphone.
func TestLightRefusedStartBlamesTheLight(t *testing.T) {
	capture := newCaptureGate(nil, nil, nil)
	capture.beforeMicStart = func() error { return indicator.ErrNoBlink1 }
	err := startMicIfUnpaused(capture, func() error { return nil })
	if warning, status := micStartFailureMessages(err); warning != "" || !strings.Contains(status, "Recording light") {
		t.Fatalf("messages for a light refusal = (%q, %q), want no microphone warning and a light status", warning, status)
	}
	if warning, _ := micStartFailureMessages(errors.New("PortAudio: device unavailable")); !strings.Contains(warning, "Microphone failed to start") {
		t.Fatalf("warning for a device failure = %q, want the microphone warning", warning)
	}
}

// TestMicRowNamesTheLightHold fails if the microphone's status row says the
// unit is paused while the light, not a person, holds capture.
func TestMicRowNamesTheLightHold(t *testing.T) {
	hub := server.NewHub()
	events := hub.Subscribe()
	defer hub.Unsubscribe(events)
	capture := newCaptureGate(nil, nil, nil)
	capture.HoldForLight()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go holdMicWhilePaused(ctx, capture, &deliveringMic{}, func(string, ...any) {}, hub)
	select {
	case raw := <-events:
		if !strings.Contains(string(raw), "light_unavailable") || strings.Contains(string(raw), "paused") {
			t.Fatalf("microphone row during a light hold = %s, want light_unavailable", raw)
		}
	case <-time.After(time.Second):
		t.Fatal("no microphone row was broadcast")
	}
}
