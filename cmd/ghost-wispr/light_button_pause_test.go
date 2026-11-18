package main

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
				controls:   &server.ControlHooks{Pause: persistent.Pause, Resume: persistent.Resume},
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
