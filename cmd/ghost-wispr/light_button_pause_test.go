package main

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/jabra"
	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// TestAResumeAfterAQueuedPauseIsNotLost fails if the last call, a resume,
// does not win when the pause before it still waited for the persistent
// controls' lock, behind another resume's save. That pause counted itself
// only once it had the lock, after the last resume read its ticket, so it
// overtook the resume (red team, review 5963682292 of #79). Control: the
// pause has returned when the resume comes.
func TestAResumeAfterAQueuedPauseIsNotLost(t *testing.T) {
	for _, tc := range []struct {
		name   string
		queued bool
	}{
		{"control: the pause has returned when the resume comes", false},
		{"the pause waits for the controls' lock when the resume comes", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capture := newCaptureGate(nil, nil, nil)
			store := &blockingPauseStore{zeroStarted: make(chan struct{}), releaseZero: make(chan struct{})}
			controls := newPersistentControls(store, capture.Pause, capture, nil)
			if err := controls.Pause(); err != nil {
				t.Fatalf("control: the first pause: %v", err)
			}
			var checks atomic.Int32
			lastEntered, releaseLast := make(chan struct{}), make(chan struct{})
			capture.beforeResumeRequest = func() error { return nil }
			capture.checkLightPattern = func() error {
				if checks.Add(1) == 2 { // the last resume's check
					close(lastEntered)
					<-releaseLast
				}
				return nil
			}
			within := func(ch <-chan error, what string) error {
				t.Helper()
				select {
				case err := <-ch:
					return err
				case <-time.After(5 * time.Second):
					t.Fatalf("control: %s did not return within 5s", what)
					return nil
				}
			}

			first := make(chan error, 1)
			go func() { first <- controls.Resume() }() // holds the controls' lock in its save
			select {
			case <-store.zeroStarted:
			case <-time.After(5 * time.Second):
				t.Fatal("control: the first resume did not begin its save")
			}
			paused := make(chan error, 1)
			go func() { paused <- controls.Pause() }() // waits for the controls' lock
			time.Sleep(50 * time.Millisecond)
			if !tc.queued {
				close(store.releaseZero)
				if err := within(first, "the first resume"); err != nil {
					t.Fatalf("control: the first resume: %v", err)
				}
				if err := within(paused, "the pause"); err != nil {
					t.Fatalf("control: the pause: %v", err)
				}
			}
			last := make(chan error, 1)
			go func() { last <- controls.Resume() }()
			select {
			case <-lastEntered:
			case <-time.After(5 * time.Second):
				t.Fatal("control: the last resume did not check the light")
			}
			if tc.queued {
				close(store.releaseZero)
				if err := within(first, "the first resume"); err != nil {
					t.Fatalf("control: the first resume: %v", err)
				}
				if err := within(paused, "the pause"); err != nil {
					t.Fatalf("control: the pause: %v", err)
				}
			}
			close(releaseLast)
			err := within(last, "the last resume")
			if stored := store.values[storage.AppStateCapturePaused]; capture.IsPaused() || stored != "0" {
				t.Fatalf("the last call, a resume called while an earlier pause waited for the controls' lock, was lost: Resume returned %v, capture paused %t, stored capture paused %q", err, capture.IsPaused(), stored)
			}
		})
	}
}

// pressRig is the power button's taps against the persistent controls and
// the capture gate of a light reopened after a device error (as
// gateOnReopenedLight), the unit paused. Its loop takes one tap at a time and
// presses the button's toggle, as runPowerButton does, so a tap waits while
// the loop is busy. pausedAt records when the armed pause reached the gate.
type pressRig struct {
	capture  *captureGate
	second   *heldLight
	store    *lockedPauseStore
	controls *server.ControlHooks
	toggle   *buttonToggle
	taps     chan struct{}

	mu       sync.Mutex
	armed    bool
	pausedAt time.Time
}

func newPressRig(t *testing.T, known bool, check time.Duration) *pressRig {
	t.Helper()
	r := &pressRig{store: &lockedPauseStore{}, taps: make(chan struct{})}
	r.capture, r.second = gateOnReopenedLight(t, known, check)
	persistent := newPersistentControls(r.store, func() {
		r.mu.Lock()
		if r.armed && r.pausedAt.IsZero() {
			r.pausedAt = time.Now()
		}
		r.mu.Unlock()
		r.capture.Pause()
	}, r.capture, nil)
	if err := persistent.Pause(); err != nil {
		t.Fatalf("control: the first pause: %v", err)
	}
	r.controls = &server.ControlHooks{Pause: persistent.Pause, Resume: persistent.Resume}
	r.toggle = newButtonToggle(r.capture, r.controls, "test button")
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		for range r.taps {
			r.toggle.press(nil, nil)
		}
	}()
	t.Cleanup(func() {
		close(r.taps)
		<-loopDone
		r.toggle.wait()
	})
	return r
}

// tap sends a tap to the loop, waiting while the loop is busy, as the power
// button's tap reader does.
func (r *pressRig) tap() { r.taps <- struct{}{} }

// stored is the saved capture pause.
func (r *pressRig) stored() string {
	value, _, _ := r.store.GetAppState(storage.AppStateCapturePaused)
	return value
}

// lockedPauseStore is a pause store whose calls may come from several
// goroutines.
type lockedPauseStore struct {
	mu sync.Mutex
	pauseStoreFake
}

func (s *lockedPauseStore) GetAppState(key string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pauseStoreFake.GetAppState(key)
}

func (s *lockedPauseStore) SetAppState(key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pauseStoreFake.SetAppState(key, value)
}

// arm records when the next pause reaches the gate.
func (r *pressRig) arm() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.armed, r.pausedAt = true, time.Time{}
}

func (r *pressRig) pauseApplied() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pausedAt
}

// TestAButtonPressDuringItsOwnResumesCheckPausesAtOnce fails if a press of a
// hardware button that pauses, made while the press before it resumes and
// checks a unit's stored pattern, reaches the gate more than 500 ms after it,
// or if the light turns red after it. The power button and the Jabra handled
// their presses one at a time, so the second press waited for the first
// press's resume, its check included, and read capture as still paused, so it
// resumed (red team, review 5963682292 of #79: 1.70 s). Each check runs
// through the power button's toggle and the Jabra's.
func TestAButtonPressDuringItsOwnResumesCheckPausesAtOnce(t *testing.T) {
	for _, tc := range gateChecks {
		t.Run("power button/"+tc.name, func(t *testing.T) {
			r := newPressRig(t, tc.known, tc.check)
			r.tap() // resume
			time.Sleep(50 * time.Millisecond)
			pressed := time.Now()
			r.arm()
			r.tap() // pause
			checkPressPaused(t, r.capture, r.second, pressed, r.pauseApplied)
			if stored := r.stored(); !r.capture.IsPaused() || stored != "1" {
				t.Fatalf("after the taps: capture paused %t, stored %q; want paused and \"1\"", r.capture.IsPaused(), stored)
			}
		})
		t.Run("Jabra/"+tc.name, func(t *testing.T) {
			capture, second := gateOnReopenedLight(t, tc.known, tc.check)
			var mu sync.Mutex
			var armed bool
			var pausedAt time.Time
			persistent := newPersistentControls(&lockedPauseStore{}, func() {
				mu.Lock()
				if armed && pausedAt.IsZero() {
					pausedAt = time.Now()
				}
				mu.Unlock()
				capture.Pause()
			}, capture, nil)
			if err := persistent.Pause(); err != nil {
				t.Fatalf("control: the first pause: %v", err)
			}
			rig := &jabraRig{
				capture:    capture,
				device:     newCaptureDevice(capture.IsPaused),
				controls:   &server.ControlHooks{Pause: persistent.Pause, Close: persistent.Close, Resume: persistent.Resume},
				warnings:   newWarningsRegistry(),
				devices:    make(chan *jabraFake, 4),
				checkEvery: time.Hour,
			}
			rig.start(t)
			device := rig.plugIn()
			waitForHardware(t, "the Jabra to connect", rig.attached)
			device.press(t) // resume
			time.Sleep(50 * time.Millisecond)
			pressed := time.Now()
			mu.Lock()
			armed = true
			mu.Unlock()
			device.press(t) // pause
			checkPressPaused(t, capture, second, pressed, func() time.Time {
				mu.Lock()
				defer mu.Unlock()
				return pausedAt
			})
		})
	}
}

// checkPressPaused fails unless a pause reached the gate within 500 ms of
// pressed, capture is paused, and no red was written after pressed.
func checkPressPaused(t *testing.T, capture *captureGate, second *heldLight, pressed time.Time, pausedAt func() time.Time) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); pausedAt().IsZero(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the second press paused nothing within 5s")
		}
	}
	took := pausedAt().Sub(pressed)
	time.Sleep(2 * time.Second) // the first press's check and its resume end
	if took > 500*time.Millisecond {
		t.Fatalf("a press to pause, 50ms into the same button's resume, reached the gate after %v, want within 500ms", took.Round(time.Millisecond))
	}
	if !capture.IsPaused() {
		t.Fatal("capture is not paused after the press to pause")
	}
	if second.redAfter(pressed) {
		t.Fatal("the light turned red after the press to pause")
	}
}

// TestAButtonPressReadsWhatThePersonLastAskedFor fails if a press made while
// the same button's resume still checks the light does not act on the state
// the person last asked for. Capture reads paused until that resume moves the
// switch. A press during it pauses. A press after a later pause, from the
// button or from the web page, resumes, and the unit then records with its
// light red.
func TestAButtonPressReadsWhatThePersonLastAskedFor(t *testing.T) {
	check := 33 * 53 * time.Millisecond
	for _, tc := range []struct {
		name  string
		pause func(*pressRig)
	}{
		{"the button paused", func(r *pressRig) { r.tap() }},
		{"the web page paused", func(r *pressRig) {
			if err := r.controls.Pause(); err != nil {
				t.Errorf("web pause: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newPressRig(t, false, check)
			r.tap() // resume: its check takes 1.75s
			time.Sleep(50 * time.Millisecond)
			tc.pause(r)
			if !r.capture.IsPaused() {
				t.Fatal("control: capture is not paused after the pause")
			}
			time.Sleep(50 * time.Millisecond)
			r.tap() // the person last asked for a pause: this resumes
			waitForHardwareWithin(t, 5*time.Second, "the last press to resume with the light red", func() bool {
				return !r.capture.IsPaused() && r.second.isRed()
			})
			r.toggle.wait()
			if stored := r.stored(); r.capture.IsPaused() || stored != "0" {
				t.Fatalf("after the last press, a resume: capture paused %t, stored %q; want recording and \"0\"", r.capture.IsPaused(), stored)
			}
		})
	}
}

// TestAButtonPressThatResumesReturnsAtOnce fails if a press that resumes holds
// up the button until its light check ends: the power button's loop and the
// Jabra's serve loop take the next press only once press has returned.
func TestAButtonPressThatResumesReturnsAtOnce(t *testing.T) {
	r := newPressRig(t, false, 33*53*time.Millisecond)
	started := time.Now()
	r.toggle.press(nil, nil)
	if took := time.Since(started); took > 200*time.Millisecond {
		t.Fatalf("a press that resumes returned after %v, want at once", took.Round(time.Millisecond))
	}
}

// TestAFailedResumeCheckDoesNotSayPausedWhileRecording fails if a resume
// whose light check failed tells the clients paused, or returns an error,
// while another resume took the switch on meanwhile: the unit records, as the
// person asked (red team, review 5963682292 of #79: statuses [true false
// true]). Control: alone on a paused unit, the failed check returns its error
// and the clients are told paused.
func TestAFailedResumeCheckDoesNotSayPausedWhileRecording(t *testing.T) {
	for _, tc := range []struct {
		name    string
		overlap bool
	}{
		{"control: the failed resume runs alone", false},
		{"another resume holds the controls' lock when the failed check returns", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capture := newCaptureGate(nil, nil, nil)
			store := &blockingPauseStore{zeroStarted: make(chan struct{}), releaseZero: make(chan struct{})}
			statuses := &statusLog{}
			controls := newPersistentControls(store, capture.Pause, capture, statuses.add)
			if err := controls.Pause(); err != nil {
				t.Fatalf("control: the pause: %v", err)
			}
			var fail atomic.Bool
			capture.beforeResumeRequest = func() error { return nil }
			capture.checkLightPattern = func() error {
				if fail.Load() {
					return errors.New("blink(1) feature report: no such device")
				}
				return nil
			}
			other := make(chan error, 1)
			if tc.overlap {
				go func() { other <- controls.Resume() }()
				select {
				case <-store.zeroStarted:
				case <-time.After(5 * time.Second):
					t.Fatal("control: the other resume did not begin its save")
				}
			} else {
				close(store.releaseZero)
			}
			fail.Store(true)
			failed := make(chan error, 1)
			go func() { failed <- controls.Resume() }()
			time.Sleep(50 * time.Millisecond) // its check has failed; it waits for the controls' lock
			if tc.overlap {
				close(store.releaseZero)
				if err := <-other; err != nil {
					t.Fatalf("control: the other resume: %v", err)
				}
			}
			err := <-failed
			got := statuses.get()
			stored := store.values[storage.AppStateCapturePaused]
			if len(got) == 0 || got[len(got)-1] != capture.IsPaused() || (stored == "1") != capture.IsPaused() {
				t.Fatalf("after the resumes, capture paused %t and saved paused %q, but the clients were last told paused %v (statuses %v)", capture.IsPaused(), stored, len(got) > 0 && got[len(got)-1], got)
			}
			if (err != nil) != capture.IsPaused() {
				t.Fatalf("the resume whose check failed returned %v with capture paused %t: want its error only while the unit stays paused", err, capture.IsPaused())
			}
		})
	}
}

// TestAJabraMuteAfterAResumeElsewherePauses fails if a press of the Jabra's
// mute button, made while the unit records, does not pause it. In the office:
// the person mutes on the Jabra; the mic loop closes the microphone, and the
// Jabra sends Phone Mute by itself just after (jabraOwnMuteWindow), which the
// guard keeps from resuming; the person unmutes from the web page or the power
// button; the person mutes on the Jabra again. The button remembered the own
// report as the person asking to stay paused, and the next press resumed the
// recording unit (red team, review 5967147417 of #79). Controls: the Jabra
// itself unmutes, and an unmute elsewhere with no own report before it.
func TestAJabraMuteAfterAResumeElsewherePauses(t *testing.T) {
	for _, tc := range []struct {
		name         string
		ownReport    bool
		jabraResumes bool
	}{
		{"control: the Jabra itself unmutes after its own report", true, true},
		{"control: the web page or power button unmutes, no own report", false, false},
		{"the Jabra's own report, then the web page or power button unmutes", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newJabraRig(false)
			button := newJabraMuteButton(rig.capture, rig.device, rig.controls, rig.warnings)
			t.Cleanup(button.toggle.wait)
			waitPaused := func(want bool, what string) {
				t.Helper()
				waitForHardware(t, what, func() bool { return rig.capture.IsPaused() == want })
			}

			rig.openMic(t) // recording
			time.Sleep(2 * jabraOwnMuteWindow)
			button.press(jabra.MonotonicNow()) // the person mutes on the Jabra
			waitPaused(true, "the person's first Jabra press to pause")
			rig.closeMic(t) // the mic loop closes the microphone for the pause
			if tc.ownReport {
				button.press(jabra.MonotonicNow()) // the Jabra's own Phone Mute, just after the close
				time.Sleep(50 * time.Millisecond)
				if !rig.capture.IsPaused() {
					t.Fatal("control: the guard took the Jabra's own report for a person's unmute")
				}
			}
			time.Sleep(2 * jabraOwnMuteWindow) // later presses are a person's
			if tc.jabraResumes {
				button.press(jabra.MonotonicNow())
			} else if err := rig.controls.Resume(); err != nil {
				t.Fatalf("control: the resume from the web page or power button: %v", err)
			}
			waitPaused(false, "the unmute")

			time.Sleep(2 * jabraOwnMuteWindow)
			button.press(jabra.MonotonicNow()) // the person mutes on the Jabra again
			for deadline := time.Now().Add(time.Second); !rig.capture.IsPaused(); time.Sleep(time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatalf("a Jabra press to mute while the unit records left it recording (own report first: %t, unmuted by the Jabra: %t): the button took the press for a resume", tc.ownReport, tc.jabraResumes)
				}
			}
		})
	}
}

// countingToggleRig is a button's toggle against the persistent controls of
// a capture gate, counting the controls' pauses and resumes.
type countingToggleRig struct {
	capture    *captureGate
	persistent *persistentControls
	toggle     *buttonToggle
	resumes    atomic.Int32
	pauses     atomic.Int32
}

func newCountingToggleRig(t *testing.T) *countingToggleRig {
	r := &countingToggleRig{capture: newCaptureGate(nil, nil, nil)}
	r.persistent = newPersistentControls(&lockedPauseStore{}, r.capture.Pause, r.capture, nil)
	r.toggle = newButtonToggle(r.capture, &server.ControlHooks{
		Pause:  func() error { r.pauses.Add(1); return r.persistent.Pause() },
		Resume: func() error { r.resumes.Add(1); return r.persistent.Resume() },
	}, "test button")
	t.Cleanup(r.toggle.wait)
	return r
}

// press presses the button and waits for the press, its resume included.
func (r *countingToggleRig) press(t *testing.T) {
	t.Helper()
	select {
	case <-r.toggle.press(nil, nil):
	case <-time.After(5 * time.Second):
		t.Fatal("control: a press did not end within 5s")
	}
}

// TestAButtonPressAfterARefusedResumeTriesAgain fails if, after a press whose
// resume the light refused, the next press of that button does not try to
// resume again. The unit is still paused, and the person sees it paused, so
// the next press is a resume; the button remembered the refused press as a
// resume and paused the paused unit instead (review 5967147417 of #79).
// Control: after a resume that worked, the next press pauses.
func TestAButtonPressAfterARefusedResumeTriesAgain(t *testing.T) {
	for _, tc := range []struct {
		name    string
		refuse  func(r *countingToggleRig)
		refused bool
	}{
		{"control: the resume is not refused", func(r *countingToggleRig) {
			r.capture.beforeResumeRequest = func() error { return nil }
		}, false},
		{"the red confirmation refuses it", func(r *countingToggleRig) {
			r.capture.beforeResumeRequest = func() error { return errors.New("recording light stayed off") }
		}, true},
		{"the pattern check refuses it", func(r *countingToggleRig) {
			r.capture.beforeResumeRequest = func() error { return nil }
			r.capture.checkLightPattern = func() error { return errors.New("blink(1) feature report: no such device") }
		}, true},
		{"the gate opening refuses it", func(r *countingToggleRig) {
			r.capture.beforeGateOpen = func() error { return errors.New("recording light went off before the gate opened") }
			r.capture.onResumeRefused = func(error) { _ = r.persistent.RefuseResume() }
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newCountingToggleRig(t)
			if err := r.persistent.Pause(); err != nil {
				t.Fatal(err)
			}
			tc.refuse(r)
			r.press(t)
			if tc.refused {
				time.Sleep(50 * time.Millisecond) // a gate opening's refusal comes after the press
				waitForHardware(t, "the refused resume to leave the unit paused", r.capture.IsPaused)
			} else {
				waitForHardware(t, "the resume", func() bool { return !r.capture.IsPaused() })
			}
			before := r.resumes.Load()
			r.press(t)
			time.Sleep(50 * time.Millisecond)
			tried := r.resumes.Load() - before
			if !tc.refused {
				if tried != 0 || !r.capture.IsPaused() {
					t.Fatalf("control: the press after a resume that worked resumed %d times, paused %t; want a pause", tried, r.capture.IsPaused())
				}
				return
			}
			if tried != 1 {
				t.Fatalf("the press after a refused resume, with the unit still paused, tried to resume %d times (and paused %d times): want 1 resume", tried, r.pauses.Load())
			}
		})
	}
}

// TestAButtonPauseLeavesNoRequestedPause fails if a button's pause leaves a
// pause counted as asked for that no Pause took: the gate then counts the unit
// as paused from then on, and a resume of a unit that records runs the light's
// pattern check. The button counted its pause, and the persistent controls
// counted it again (review 5967147417 of #79). Control: the same pause from
// the web page.
func TestAButtonPauseLeavesNoRequestedPause(t *testing.T) {
	for _, viaButton := range []bool{false, true} {
		name := "control: a pause from the web page"
		if viaButton {
			name = "a pause from a button"
		}
		t.Run(name, func(t *testing.T) {
			r := newCountingToggleRig(t)
			if viaButton {
				r.press(t)
			} else if err := r.persistent.Pause(); err != nil {
				t.Fatal(err)
			}
			if !r.capture.IsPaused() {
				t.Fatal("control: the pause did not pause")
			}
			r.capture.mu.Lock()
			requested := r.capture.requestedPauses
			r.capture.mu.Unlock()
			if err := r.persistent.Resume(); err != nil {
				t.Fatalf("control: the resume: %v", err)
			}
			waitForHardware(t, "the resume", func() bool { return !r.capture.IsPaused() })
			var checks atomic.Int32
			r.capture.beforeResumeRequest = func() error { return nil }
			r.capture.checkLightPattern = func() error { checks.Add(1); return nil }
			if err := r.persistent.Resume(); err != nil { // a second click on a recording unit
				t.Fatalf("a resume of a recording unit: %v", err)
			}
			if requested != 0 || checks.Load() != 0 {
				t.Fatalf("after one pause, %d requested pauses were left that no Pause took, and a resume of a unit that records ran the light's pattern check %d times; want 0 and 0", requested, checks.Load())
			}
		})
	}
}

// TestAJabraPressActsOnTheRingItIsPressedOn fails if a press of the Jabra's
// mute button does the opposite of what the ring it is pressed on shows: a
// press on a ring that shows Muted must unmute, one on an unmuted ring must
// mute. The person unmutes a paused unit on the Jabra; while that press's
// resume checks the light, Ghost Wispr wrote the ring Muted again from the
// pause switch, which the resume had not moved yet, but the button counted
// the unit as resumed, so the person's second press, on a Muted ring, paused
// (red team, review 5968112897 of #79). Control: a unit known black, whose
// resume ends before the second press.
func TestAJabraPressActsOnTheRingItIsPressedOn(t *testing.T) {
	for _, tc := range gateChecks {
		t.Run(tc.name, func(t *testing.T) {
			capture, _ := gateOnReopenedLight(t, tc.known, tc.check)
			persistent := newPersistentControls(&lockedPauseStore{}, capture.Pause, capture, nil)
			if err := persistent.Pause(); err != nil {
				t.Fatalf("control: the first pause: %v", err)
			}
			rig := &jabraRig{
				capture:    capture,
				device:     newCaptureDevice(capture.IsPaused),
				controls:   &server.ControlHooks{Pause: persistent.Pause, Close: persistent.Close, Resume: persistent.Resume},
				warnings:   newWarningsRegistry(),
				devices:    make(chan *jabraFake, 4),
				checkEvery: time.Hour,
			}
			rig.start(t)
			device := rig.plugIn()
			waitForHardware(t, "the Jabra ring to show the pause", func() bool { return rig.attached() && device.shows() })

			// The person presses mute on a ring that shows Muted: the Jabra
			// flips its own ring, and the press reaches the button.
			device.lightsItself(false)
			device.press(t)
			time.Sleep(150 * time.Millisecond)
			ringMuted := device.shows()

			// The person presses again, on what the ring shows now.
			device.lightsItself(!ringMuted)
			device.press(t)
			time.Sleep(tc.check + time.Second) // the first press's check and resume end
			if paused := capture.IsPaused(); paused == ringMuted {
				t.Fatalf("the second Jabra press, made on a ring that showed Muted %t (as Ghost Wispr wrote it 150ms after the first press), left capture paused %t: a press on a Muted ring must unmute, on an unmuted ring must mute", ringMuted, paused)
			}
		})
	}
}

// TestAJabraRingShowsMutedAgainOnceItsResumeIsRefused fails if the Jabra's
// ring does not show unmuted while its press's resume checks the light, or
// does not show Muted again once the light refuses that resume: the unit is
// then still paused. Control: a resume that is not refused leaves the ring
// unmuted.
func TestAJabraRingShowsMutedAgainOnceItsResumeIsRefused(t *testing.T) {
	for _, refused := range []bool{false, true} {
		name := "control: the resume is not refused"
		if refused {
			name = "the red confirmation refuses the resume"
		}
		t.Run(name, func(t *testing.T) {
			rig := newJabraRig(true)
			rig.checkEvery = time.Hour
			release := make(chan struct{})
			var releaseOnce sync.Once
			rig.capture.beforeResumeRequest = func() error {
				<-release
				if refused {
					return errors.New("recording light stayed off")
				}
				return nil
			}
			rig.start(t)
			// Runs before the rig's cleanup, which waits for the press.
			t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
			device := rig.plugIn()
			waitForHardware(t, "the Jabra ring to show the pause", func() bool { return rig.attached() && device.shows() })

			device.lightsItself(false) // the Jabra flips its own ring on the press
			device.press(t)
			time.Sleep(50 * time.Millisecond)
			if device.shows() {
				t.Fatal("the Jabra ring showed Muted while its press's resume checked the light: the button counts the unit as resumed")
			}
			releaseOnce.Do(func() { close(release) })
			if refused {
				waitForHardware(t, "the ring to show Muted again once the light refused the resume", device.shows)
				if !rig.capture.IsPaused() {
					t.Fatal("control: the refused resume left the unit recording")
				}
				return
			}
			waitForHardware(t, "the resume", func() bool { return !rig.capture.IsPaused() })
			time.Sleep(50 * time.Millisecond)
			if device.shows() {
				t.Fatal("control: the ring showed Muted after a resume that worked")
			}
		})
	}
}

// TestAnOldResumeEndingDoesNotEndTheNewOne fails if a button's resume that
// its own pause overtook, ending while a later resume of that button still
// checks the light, makes the button stop counting the later one: a press
// during the later resume's check would then be taken for a resume. Presses:
// A resumes, B pauses during A's check, C resumes, D comes during C's check
// and must pause (review 5968112897 of #79). Control: A ends after D.
func TestAnOldResumeEndingDoesNotEndTheNewOne(t *testing.T) {
	for _, tc := range []struct {
		name           string
		firstEndsFirst bool
	}{
		{"control: the first resume ends after the last press", false},
		{"the first resume ends before the last press", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newCountingToggleRig(t)
			if err := r.persistent.Pause(); err != nil {
				t.Fatal(err)
			}
			entered := []chan struct{}{make(chan struct{}), make(chan struct{})}
			release := []chan struct{}{make(chan struct{}), make(chan struct{})}
			var releases [2]sync.Once
			let := func(i int) { releases[i].Do(func() { close(release[i]) }) }
			var calls atomic.Int32
			r.capture.beforeResumeRequest = func() error { return nil }
			r.capture.checkLightPattern = func() error {
				if i := int(calls.Add(1)) - 1; i < 2 {
					close(entered[i])
					<-release[i]
				}
				return nil
			}
			// Runs before the rig's cleanup, which waits for the resumes.
			t.Cleanup(func() { let(0); let(1) })
			waitFor := func(ch <-chan struct{}, what string) {
				t.Helper()
				select {
				case <-ch:
				case <-time.After(5 * time.Second):
					t.Fatalf("control: %s did not happen within 5s", what)
				}
			}

			first := r.toggle.press(nil, nil) // A, a resume
			waitFor(entered[0], "resume A's check")
			waitFor(r.toggle.press(nil, nil), "pause B")
			if !r.capture.IsPaused() {
				t.Fatal("control: pause B did not pause")
			}
			r.toggle.press(nil, nil) // C, a resume
			waitFor(entered[1], "resume C's check")
			if tc.firstEndsFirst {
				let(0)
				waitFor(first, "resume A to end")
			}
			before := r.resumes.Load()
			last := r.toggle.press(nil, nil) // D, during C's check
			if !tc.firstEndsFirst {
				let(0)
				waitFor(first, "resume A to end")
			}
			let(1)
			waitFor(last, "press D")
			r.toggle.wait()
			if resumed := r.resumes.Load() - before; resumed != 0 || !r.capture.IsPaused() {
				t.Fatalf("press D, made during resume C's check, was taken for a resume (resumes after it %d), and the unit ends paused %t: want D a pause and the unit paused", resumed, r.capture.IsPaused())
			}
		})
	}
}
