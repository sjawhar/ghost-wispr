package main

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/indicator"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// statusLog records the paused statuses the persistent controls broadcast.
type statusLog struct {
	mu       sync.Mutex
	statuses []bool
}

func (s *statusLog) add(paused bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statuses = append(s.statuses, paused)
}

func (s *statusLog) get() []bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.statuses)
}

// TestAPersonsPauseDuringAResumesPatternCheckWins fails if a pause a person
// taps while their resume checks a unit's stored pattern waits more than
// 500 ms, if the light turns red after the tap, or if the unit is not paused,
// saved paused and last told paused afterwards. The persistent controls,
// which the web API, the power button and the Jabra call, held their lock
// across the gate's resume, its check included, so the pause waited for the
// check and came after it (red team, review 5961636823 of #79: 1.71 s, red
// after the tap). Each check runs through the persistent controls and,
// as a control, on the gate directly.
func TestAPersonsPauseDuringAResumesPatternCheckWins(t *testing.T) {
	for _, path := range []struct {
		name     string
		controls bool
	}{
		{"control: the gate called directly", false},
		{"the persistent controls", true},
	} {
		for _, tc := range gateChecks {
			t.Run(path.name+"/"+tc.name, func(t *testing.T) {
				capture, second := gateOnReopenedLight(t, tc.known, tc.check)
				store := &pauseStoreFake{}
				statuses := &statusLog{}
				pause := func() error { capture.Pause(); return nil }
				resume := capture.RequestResume
				if path.controls {
					controls := newPersistentControls(store, capture.Pause, capture, statuses.add)
					pause, resume = controls.Pause, controls.Resume
				}
				if err := pause(); err != nil {
					t.Fatalf("control: the first pause: %v", err)
				}
				resumed := make(chan error, 1)
				go func() { resumed <- resume() }()
				time.Sleep(50 * time.Millisecond) // the person taps resume, then pause

				tapped := time.Now()
				if err := pause(); err != nil {
					t.Fatalf("control: the pause: %v", err)
				}
				if took := time.Since(tapped); took > 500*time.Millisecond {
					t.Fatalf("a pause tapped 50ms into a resume returned after %v, want within 500ms", took.Round(time.Millisecond))
				}
				if err := <-resumed; err != nil {
					t.Fatalf("the resume: %v, want nil: a later pause overtook it", err)
				}
				time.Sleep(300 * time.Millisecond) // a gate opening the resume started, if any
				if !capture.IsPaused() {
					t.Fatal("capture is not paused after the pause")
				}
				if second.redAfter(tapped) {
					t.Fatal("the light turned red after the person tapped pause")
				}
				if !path.controls {
					return
				}
				if got := store.values[storage.AppStateCapturePaused]; got != "1" {
					t.Fatalf("stored capture pause after the pause = %q, want 1", got)
				}
				if got := statuses.get(); len(got) == 0 || !got[len(got)-1] {
					t.Fatalf("status broadcasts %v, want the last one paused", got)
				}
			})
		}
	}
}

// TestAResumeRunsTheLightCheckOnlyForAPausedUnit fails if a resume of a unit
// whose switch is on runs the light's pattern check, or if a resume of a
// paused unit, or of one whose pause is still waiting for the gate, does not.
// A check that fails on an unpaused unit made the persistent controls save
// capture paused and tell the clients paused while capture stayed on (red
// team, review 5961636823 of #79).
func TestAResumeRunsTheLightCheckOnlyForAPausedUnit(t *testing.T) {
	for _, tc := range []struct {
		name      string
		setup     func(*captureGate) (release func())
		wantCheck bool
	}{
		{"the switch is on", func(*captureGate) func() { return func() {} }, false},
		{"control: the unit is paused", func(g *captureGate) func() { g.Pause(); return func() {} }, true},
		{"control: a pause waits for the gate", func(g *captureGate) func() {
			g.control.Lock()
			paused := make(chan struct{})
			go func() { g.Pause(); close(paused) }()
			time.Sleep(50 * time.Millisecond) // the pause waits for control
			return func() { g.control.Unlock(); <-paused }
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capture := newCaptureGate(nil, nil, nil)
			var checks atomic.Int32
			capture.beforeResumeRequest = func() error { return nil }
			capture.checkLightPattern = func() error { checks.Add(1); return errors.New("blink(1) feature report: no such device") }
			release := tc.setup(capture)
			done := make(chan error, 1)
			go func() { done <- capture.RequestResume() }()
			time.Sleep(50 * time.Millisecond)
			release()
			err := <-done
			if ran := checks.Load() > 0; ran != tc.wantCheck {
				t.Fatalf("the resume ran the light check %t (err %v), want %t", ran, err, tc.wantCheck)
			}
			if tc.wantCheck != (err != nil) {
				t.Fatalf("the resume = %v, want an error %t", err, tc.wantCheck)
			}
		})
	}
}

// TestAResumeOfAnUnpausedUnitPersistsNoPause fails if a resume sent to a unit
// whose switch is on (a second click, a stale page) saves capture paused or
// tells the clients paused while the switch stays on: the light's check ran
// before the switch was looked at, and its error was taken for a refused
// resume (red team, review 5961636823 of #79). Control: the light is there.
func TestAResumeOfAnUnpausedUnitPersistsNoPause(t *testing.T) {
	for _, tc := range []struct {
		name string
		open func(string, string) (indicator.Light, error)
	}{
		{"control: the light is there", func(string, string) (indicator.Light, error) {
			return &reconnectLightFake{colour: [3]uint8{255, 0, 0}}, nil
		}},
		{"the light is unplugged", func(string, string) (indicator.Light, error) {
			return nil, indicator.ErrNoBlink1
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := openBlink1
			openBlink1 = tc.open
			t.Cleanup(func() { openBlink1 = original })
			warnings := newWarningsRegistry()
			light := &reconnectingLight{warnings: warnings, retryEvery: time.Hour}
			hardware := &hardwareControl{light: light}
			light.holdRed = hardware.holdsRed
			capture := newCaptureGate(nil, nil, nil)
			wireLightConsent(capture, hardware, warnings, func(error) {})
			store := &pauseStoreFake{}
			statuses := &statusLog{}
			controls := newPersistentControls(store, capture.Pause, capture, statuses.add)

			err := controls.Resume()
			if capture.IsPaused() {
				t.Fatal("control: a resume paused the capture switch")
			}
			if stored := store.values[storage.AppStateCapturePaused]; stored == "1" || slices.Contains(statuses.get(), true) {
				t.Fatalf("a resume of a unit that was not paused returned %v, saved capture paused %q and told clients paused %v, while the switch stayed on", err, stored, statuses.get())
			}
		})
	}
}

// TestAPauseDuringAMicOpensPatternCheckOpensNothing fails if a microphone open
// (confirmMicOpen) whose light check a pause came during confirms the light
// red, or returns anything but errMicStartPaused: the mic loop then opens the
// microphone of a paused unit, and the light turns red after the pause
// (review 5961636823 of #79: 1.65 s after it). Control: a unit known black
// needs no check, and its open is not cancelled.
func TestAPauseDuringAMicOpensPatternCheckOpensNothing(t *testing.T) {
	for _, tc := range gateChecks[:2] {
		t.Run(tc.name, func(t *testing.T) {
			capture, second := gateOnReopenedLight(t, tc.known, tc.check)
			confirmed := make(chan error, 1)
			go func() { confirmed <- capture.confirmMicOpen() }()
			time.Sleep(100 * time.Millisecond)
			paused := time.Now()
			capture.Pause()
			err := <-confirmed
			if err == nil {
				capture.finishMicStart()
			}
			if tc.known {
				return // the open confirmed red before the pause; the controller turns it off
			}
			if !errors.Is(err, errMicStartPaused) {
				t.Fatalf("confirmMicOpen with a pause during its check = %v, want errMicStartPaused", err)
			}
			if second.redAfter(paused) {
				t.Fatal("the light turned red after the pause")
			}
		})
	}
}

// pausingCheck is a light check that pauses capture the first time it runs,
// as a person's pause does during the check, and passes.
func pausingCheck(capture *captureGate) func() error {
	var once sync.Once
	return func() error {
		once.Do(capture.Pause)
		return nil
	}
}

// TestTheMicLoopOpensNothingForAPauseDuringItsLightCheck fails if the mic
// loop, opening the microphone again on resume, opens it when a pause came
// during its light check, or counts that as a light it cannot confirm and
// waits its backoff before the next try. The next resume opens it.
func TestTheMicLoopOpensNothingForAPauseDuringItsLightCheck(t *testing.T) {
	capture := newCaptureGate(nil, nil, nil)
	capture.Pause()
	var unconfirmed atomic.Int32
	capture.onLightUnconfirmed = func(error) { unconfirmed.Add(1) }
	mic := newScriptedMic()
	waits := &recordedWaits{}
	runMicLoop(t, capture, mic, io.Discard, waits.wait, nil)
	mic.expectEvents(t, "close")

	capture.checkLightPattern = pausingCheck(capture)
	capture.Resume()
	mic.expectNoEvents(t)
	if !capture.IsPaused() {
		t.Fatal("control: the check's pause did not pause capture")
	}
	if n := unconfirmed.Load(); n != 0 {
		t.Fatalf("a pause during the check counted as an unconfirmed light %d times, want 0", n)
	}
	if got := waits.get(); len(got) != 0 {
		t.Fatalf("the loop waited %v after a pause during its light check, want no backoff", got)
	}

	capture.Resume()
	mic.expectEvents(t, "open", "stream")
}

// TestTheMicLoopReopensNothingForAPauseDuringItsLightCheck fails if the mic
// loop, reopening the microphone after a stream error, reopens it when a
// pause came during its light check: it must close it, and the loop top waits
// for the next resume.
func TestTheMicLoopReopensNothingForAPauseDuringItsLightCheck(t *testing.T) {
	capture := newCaptureGate(nil, nil, nil)
	var unconfirmed atomic.Int32
	capture.onLightUnconfirmed = func(error) { unconfirmed.Add(1) }
	capture.checkLightPattern = pausingCheck(capture)
	mic := newScriptedMic()
	mic.streamErrs = []error{errors.New("device disconnected")}
	runMicLoop(t, capture, mic, io.Discard, (&recordedWaits{}).wait, nil)
	// The loop closes the device before its reconnect wait, and again, as a
	// light refusal does, when the pause cancels the reconnect.
	mic.expectEvents(t, "stream", "close", "close")
	mic.expectNoEvents(t)
	if !capture.IsPaused() {
		t.Fatal("control: the check's pause did not pause capture")
	}
	if n := unconfirmed.Load(); n != 0 {
		t.Fatalf("a pause during the check counted as an unconfirmed light %d times, want 0", n)
	}
}

// TestReopenMicLogsAPauseDuringItsLightCheckAsAPause fails if a reconnect
// whose light check a pause came during reopens the device, or logs it as a
// light it cannot confirm.
func TestReopenMicLogsAPauseDuringItsLightCheckAsAPause(t *testing.T) {
	capture := newCaptureGate(nil, nil, nil)
	capture.checkLightPattern = pausingCheck(capture)
	mic := newScriptedMic()
	var logs []string
	confirmed, err := reopenMic(capture, mic, func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }, nil)
	if confirmed || err != nil {
		t.Fatalf("reopenMic with a pause during its check = %t, %v; want false, nil", confirmed, err)
	}
	mic.expectEvents(t, "close")
	mic.expectNoEvents(t)
	for _, line := range logs {
		if strings.Contains(line, "light unavailable") {
			t.Fatalf("reopenMic logged %q for a pause during its light check", line)
		}
	}
}

// TestAPauseDuringAMicOpensCheckLeavesTheNextResumeResuming fails if the mic
// loop, when a pause cancels its open during the light's check, ends the
// resuming state of a resume that came after that pause: capture then reads
// mic_unavailable, not resuming, until the loop's next open has finished. The
// loop called finishResume for the cancelled open (review of #93 by its
// reviewer). Control: the state right after the later resume opened the gate.
func TestAPauseDuringAMicOpensCheckLeavesTheNextResumeResuming(t *testing.T) {
	capture := newCaptureGate(nil, nil, nil)
	entered := []chan struct{}{make(chan struct{}), make(chan struct{})}
	release := []chan struct{}{make(chan struct{}), make(chan struct{})}
	var releases [2]sync.Once
	let := func(i int) { releases[i].Do(func() { close(release[i]) }) }
	var calls atomic.Int32
	capture.checkLightPattern = func() error {
		if i := int(calls.Add(1)) - 1; i < 2 {
			close(entered[i])
			<-release[i]
		}
		return nil
	}
	mic := newScriptedMic()
	runMicLoop(t, capture, mic, io.Discard, func(time.Duration) {}, nil)
	// Runs before the loop's cleanup, which waits for the loop.
	t.Cleanup(func() { let(0); let(1) })
	waitFor := func(ch <-chan struct{}, what string) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(testWait):
			t.Fatalf("control: %s did not happen within %v", what, testWait)
		}
	}

	mic.expectEvents(t, "stream")
	capture.Pause()
	mic.capture(t, []byte{1})
	mic.expectEvents(t, "close")
	capture.Resume()
	waitFor(entered[0], "the open's light check")
	capture.Pause() // cancels that open
	capture.Resume()
	waitOpen(t, capture)
	if got := capture.MicHold(); got != captureResuming {
		t.Fatalf("control: right after the later resume opened the gate, mic hold = %q, want %q", got, captureResuming)
	}
	let(0) // the cancelled open returns
	waitFor(entered[1], "the next open's light check")
	if got := capture.MicHold(); got != captureResuming {
		t.Fatalf("while the open after a cancelled one checks the light, mic hold = %q, want %q: the cancelled open ended the later resume's state", got, captureResuming)
	}
	let(1)
	mic.expectEvents(t, "open", "stream")
}
