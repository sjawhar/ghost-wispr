package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/indicator"
	"github.com/sjawhar/ghost-wispr/internal/jabra"
	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/wake/local"
)

// The three states of the microphone (INFRA-4592 R1, the control table):
//
//	state      light  Jabra ring  microphone
//	recording  red    not muted   open, through the capture gate
//	listening  off    not muted   open, the paused listener only
//	closed     off    muted       closed
//
// micStatesRig is that machine as main wires it: the recording light and its
// consent check, the persistent controls, the capture device, the mic loop on
// a fake microphone, the power button's toggle, the Jabra's mute button on a
// fake Jabra, and the paused listener on a fake ALSA capture.

// phraseSample marks the fake capture's audio of "Wispr, start recording":
// the fake spotter hears a buffer that holds it.
const phraseSample = 4242

// phraseSpotter is stage 1: it hears phraseSample.
type phraseSpotter struct{ accepted atomic.Int64 }

func (s *phraseSpotter) Accept(samples []float32) bool {
	s.accepted.Add(int64(len(samples)))
	return slices.Contains(samples, float32(phraseSample)/32768)
}

func (*phraseSpotter) Reset() {}
func (*phraseSpotter) Close() {}

// startVerifier is stage 2, which confirms every hit.
type startVerifier struct{}

func (startVerifier) Text([]float32) string { return "WHISPER START RECORDING" }
func (startVerifier) Close()                {}

// statesLight is a blink(1) that records its operations, and that reads back
// off while refuse is set, as a light whose LED failed: a red confirmation
// then fails.
type statesLight struct {
	opsLight
	refuse atomic.Bool
}

func (l *statesLight) ReadRGB() (uint8, uint8, uint8, error) {
	r, g, b, err := l.opsLight.ReadRGB()
	if l.refuse.Load() {
		return 0, 0, 0, err
	}
	return r, g, b, err
}

func (l *statesLight) red() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.colour == [3]uint8{255, 0, 0}
}

// statesMic is the recording microphone for the mic loop: the light reads it
// open from each open until its close. Stream blocks until the close. With
// stuck set, a close cannot release it, as one whose read still runs
// (audio.ErrMicStillReading), until unstick.
type statesMic struct {
	r       *micStatesRig
	mu      sync.Mutex
	closed  chan struct{}
	stuck   bool
	unstuck chan struct{}
}

func (m *statesMic) open() error {
	m.r.note("mic open")
	m.mu.Lock()
	m.closed = make(chan struct{})
	m.mu.Unlock()
	m.r.mic.open.Store(true)
	return nil
}

func (m *statesMic) Open() error   { return m.open() }
func (m *statesMic) Reopen() error { return m.open() }
func (m *statesMic) Start() error  { return nil }

func (m *statesMic) Close() error {
	m.mu.Lock()
	stuck, unstuck := m.stuck, m.unstuck
	m.mu.Unlock()
	if stuck {
		select {
		case <-unstuck:
		default:
			return audio.ErrMicStillReading
		}
	}
	m.r.mic.open.Store(false)
	m.r.note("mic close")
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed != nil {
		select {
		case <-m.closed:
		default:
			close(m.closed)
		}
	}
	return nil
}

func (m *statesMic) Stream(io.Writer) error {
	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	if closed == nil {
		return errors.New("stream before open")
	}
	<-closed
	return errors.New("stream closed")
}

// statesCapture is the listener's ALSA capture: each read takes 10 ms and
// gives the phrase while the rig speaks it, else silence.
type statesCapture struct{ r *micStatesRig }

func (c statesCapture) Read(pcm []int16) (int, error) {
	time.Sleep(10 * time.Millisecond)
	fill := int16(0)
	if c.r.speaking.Load() {
		fill = phraseSample
	}
	for i := range pcm {
		pcm[i] = fill
	}
	return len(pcm), nil
}

func (c statesCapture) Close() error {
	c.r.listenerOpen.Store(false)
	c.r.note("listener close")
	return nil
}

type micStatesRig struct {
	light    *statesLight
	capture  *captureGate
	hardware *hardwareControl
	mic      *openMicFake
	recMic   *statesMic
	device   *captureDevice
	controls *persistentControls
	hooks    *server.ControlHooks
	store    *lockedPauseStore
	warnings *warningsRegistry
	power    *buttonToggle
	jabra    *jabraFake
	jabras   chan *jabraFake // the Jabras plugged in, one per connect
	spotter  *phraseSpotter
	cfgStore *config.Store

	listenerOpen atomic.Bool
	speaking     atomic.Bool
	openFails    atomic.Bool

	mu         sync.Mutex
	events     []string
	violations []string
}

// note records a stream event in order, and the handover's rule: the
// recording microphone opens only while the listener's stream is closed, and
// the listener's opens only while the recording microphone's is closed.
func (r *micStatesRig) note(event string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
	switch {
	case event == "mic open" && r.listenerOpen.Load():
		r.violations = append(r.violations, "the recording microphone opened with the listener's stream open")
	case event == "listener open" && r.mic.open.Load():
		r.violations = append(r.violations, "the listener's stream opened with the recording microphone open")
	case event == "scan" && r.listenerOpen.Load():
		r.violations = append(r.violations, "PortAudio scanned with the listener's stream open")
	case event == "prepare" && r.listenerOpen.Load():
		r.violations = append(r.violations, "the recording microphone prepared to open with the listener's stream open")
	}
}

func (r *micStatesRig) requireHandoverKept(t *testing.T) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.violations) > 0 {
		t.Fatalf("handover broken: %v; stream events %v", r.violations, r.events)
	}
}

type statesOptions struct {
	listener bool   // voice.paused_listener
	jabra    bool   // a Jabra's mute button is attached
	saved    string // the saved state at start, "" for none
}

// startMicStatesRig starts the machine. With no saved state it starts
// recording, with the light red and the recording microphone open.
func startMicStatesRig(t *testing.T, opts statesOptions) *micStatesRig {
	t.Helper()
	r := &micStatesRig{light: &statesLight{}, mic: &openMicFake{}, warnings: newWarningsRegistry(), spotter: &phraseSpotter{}}
	r.store = &lockedPauseStore{}
	if opts.saved != "" {
		r.store.values = map[string]string{storage.AppStateCapturePaused: opts.saved}
	}
	originalOpen, originalCheck, originalRetry := openBlink1, lightCheckEvery, lightRetryEvery
	openBlink1 = func(string, string) (indicator.Light, error) { return r.light, nil }
	lightCheckEvery = 20 * time.Millisecond
	lightRetryEvery = 5 * time.Millisecond
	r.capture = newCaptureGate(nil, nil, nil)
	saved, err := restorePause(r.store, r.capture)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var loops sync.WaitGroup
	t.Cleanup(func() {
		cancel()
		_ = r.recMic.Close()
		loops.Wait()
		r.hardware.Stop()
		<-r.hardware.Done()
		openBlink1, lightCheckEvery, lightRetryEvery = originalOpen, originalCheck, originalRetry
	})
	r.hardware = startHardware(ctx, &config.Config{IndicatorLight: config.IndicatorLightBlink1}, r.capture, r.mic, r.warnings, server.NewHub())
	r.hooks = &server.ControlHooks{IsPaused: r.capture.IsPaused}
	r.controls = newPersistentControls(r.store, func() { pauseCapture(r.capture, r.hardware) }, r.capture, nil)
	wirePauseControls(r.hooks, r.controls, r.capture, r.hardware, r.warnings)
	r.device = newCaptureDevice(r.capture.inClosedState)
	r.capture.beforeMicOpen = func(ctx context.Context, closing <-chan struct{}) bool {
		r.note("prepare")
		return r.device.prepareLightForOpen(ctx, closing)
	}
	r.power = newButtonToggle(r.capture, r.hooks, "power-button")

	r.jabras = make(chan *jabraFake, 4)
	if opts.jabra {
		r.jabra = newJabraFake()
		r.jabras <- r.jabra
		button := newJabraMuteButton(r.capture, r.device, r.hooks, r.warnings)
		button.connect = func() (io.ReadWriteCloser, error) {
			select {
			case device := <-r.jabras:
				return device, nil
			default:
				return nil, jabra.ErrNoMuteButton
			}
		}
		button.retryEvery, button.checkEvery = time.Millisecond, time.Millisecond
		loops.Go(func() { button.run(ctx) })
		waitForStates(t, "the Jabra to attach", r.device.jabraAttached)
	}

	cfgStore, _, err := config.NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	if err := cfgStore.Update(func(c *config.Config) { c.Voice.PausedListener = opts.listener }); err != nil {
		t.Fatal(err)
	}
	r.cfgStore = cfgStore
	startPausedListener(ctx, cfgStore, pausedListenerUnit{
		capture: r.capture, controls: r.controls, hooks: r.hooks, device: r.device,
		wake: r.hardware.Wake, warnings: r.warnings, restored: saved, built: true,
		newDetector: func() (*local.Detector, error) { return local.NewDetectorOf(r.spotter, startVerifier{}), nil },
		openCapture: func() (local.Capture, error) {
			if r.openFails.Load() {
				return nil, errors.New("ALSA: open plughw:CARD=J75,DEV=0 for capture: Device or resource busy")
			}
			r.listenerOpen.Store(true)
			r.note("listener open")
			return statesCapture{r: r}, nil
		},
	})

	r.recMic = &statesMic{r: r}
	if saved == micRecording {
		// As startMicrophone does: the microphone opens under a red
		// confirmation, then the mic loop streams it.
		if err := r.capture.openUnderRed(r.recMic.open); err != nil {
			t.Fatal(err)
		}
	}
	loops.Go(func() {
		streamMicWithRetry(ctx, r.capture, orderedMic{micStreamer: r.recMic, device: r.device}, io.Discard, func(time.Duration) {}, nopLogf, nil)
	})
	if saved == micRecording {
		r.requireState(t, micRecording, "at start")
	}
	if opts.listener && opts.jabra {
		// The spotter loads in the background; until it has, every pause
		// closes the microphone.
		waitForStates(t, "the paused listener to start", r.controls.canListen)
	}
	return r
}

// waitForStates waits up to testWait for cond.
func waitForStates(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(testWait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// observed is what a person in the room, and the status, can tell of the
// state.
type observed struct {
	state                 micState
	red, muted            bool
	micOpen, listenerOpen bool
}

func (o observed) String() string {
	return fmt.Sprintf("state %v, light red %t, ring muted %t, recording microphone open %t, listener open %t", o.state, o.red, o.muted, o.micOpen, o.listenerOpen)
}

func (r *micStatesRig) observe() observed {
	// A light that reads back off shows no red, whatever was written to it.
	red := r.light.red() && !r.light.refuse.Load()
	o := observed{state: r.capture.micState(), red: red, micOpen: r.mic.open.Load(), listenerOpen: r.listenerOpen.Load()}
	o.muted = r.device.ringMuted()
	if r.jabra != nil {
		o.muted = r.jabra.shows()
	}
	return o
}

// want is what each state looks like once the machine has settled into it.
func want(state micState) observed {
	switch state {
	case micRecording:
		return observed{state: micRecording, red: true, micOpen: true}
	case micListening:
		return observed{state: micListening, listenerOpen: true}
	default:
		return observed{state: micClosed, muted: true}
	}
}

// requireState waits for the machine to settle into state, and fails with
// what it shows if it does not.
func (r *micStatesRig) requireState(t *testing.T, state micState, after string) {
	t.Helper()
	deadline := time.Now().Add(testWait)
	for {
		got := r.observe()
		if got == want(state) {
			r.requireHandoverKept(t)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %v; want %v (warnings %q)", after, got, want(state), r.warnings.List())
		}
		r.hardware.Wake()
		time.Sleep(2 * time.Millisecond)
	}
}

// saidPhrase says "Wispr, start recording" for 200 ms.
func (r *micStatesRig) saidPhrase() {
	r.speaking.Store(true)
	time.Sleep(200 * time.Millisecond)
	r.speaking.Store(false)
}

// pressJabra presses the Jabra's mute button, outside every own operation's
// cover, as a person does.
func (r *micStatesRig) pressJabra(t *testing.T) {
	t.Helper()
	time.Sleep(2 * jabraOwnMuteWindow)
	r.jabra.press(t)
}

func (r *micStatesRig) saved() string {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	return r.store.values[storage.AppStateCapturePaused]
}

// moveTo brings a recording rig to state through the page.
func (r *micStatesRig) moveTo(t *testing.T, state micState) {
	t.Helper()
	switch state {
	case micListening:
		if err := r.hooks.Pause(); err != nil {
			t.Fatal(err)
		}
	case micClosed:
		if err := r.hooks.Close(); err != nil {
			t.Fatal(err)
		}
	}
	r.requireState(t, state, "moving to "+state.String())
}

// TestMicStatesFollowEachControl fails if any cell of the control table is
// not what the machine does: from each of the three states, each control
// moves it to the state the table names, and the light, the Jabra's ring and
// the microphones then show that state.
func TestMicStatesFollowEachControl(t *testing.T) {
	controls := []struct {
		name string
		do   func(t *testing.T, r *micStatesRig)
	}{
		{"the page's pause", func(t *testing.T, r *micStatesRig) { _ = r.hooks.Pause() }},
		{"the page's resume", func(t *testing.T, r *micStatesRig) { _ = r.hooks.Resume() }},
		{"the power button", func(t *testing.T, r *micStatesRig) { <-r.power.press(nil, nil) }},
		{"the Jabra's mute button", func(t *testing.T, r *micStatesRig) { r.pressJabra(t) }},
		{"Wispr, start recording", func(t *testing.T, r *micStatesRig) { r.saidPhrase() }},
		{"POST /api/close", func(t *testing.T, r *micStatesRig) { _ = r.hooks.Close() }},
	}
	table := map[micState][]micState{
		//                pause         resume        power         Jabra         phrase        close
		micRecording: {micListening, micRecording, micListening, micClosed, micRecording, micClosed},
		micListening: {micListening, micRecording, micRecording, micClosed, micRecording, micClosed},
		micClosed:    {micClosed, micRecording, micRecording, micRecording, micClosed, micClosed},
	}
	for _, from := range []micState{micRecording, micListening, micClosed} {
		for i, control := range controls {
			t.Run(fmt.Sprintf("%v, %s", from, control.name), func(t *testing.T) {
				r := startMicStatesRig(t, statesOptions{listener: true, jabra: true})
				r.moveTo(t, from)
				control.do(t, r)
				r.requireState(t, table[from][i], fmt.Sprintf("%s from %v", control.name, from))
				if got, want := r.saved(), savedValue(table[from][i]); from != table[from][i] && got != want {
					t.Fatalf("saved state %q after %s from %v, want %q", got, control.name, from, want)
				}
			})
		}
	}
}

// TestListenerOffIsTodaysMachine fails if, with voice.paused_listener off, the
// unit is not the two-state machine it was before the listener: every pause
// closes the microphone, the ring shows Muted for every pause, and a Jabra
// press after a page pause resumes recording.
func TestListenerOffIsTodaysMachine(t *testing.T) {
	r := startMicStatesRig(t, statesOptions{listener: false, jabra: true})
	if err := r.hooks.Pause(); err != nil {
		t.Fatal(err)
	}
	r.requireState(t, micClosed, "a page pause with the listener off")
	if got := r.saved(); got != savedClosed {
		t.Fatalf("saved %q after the pause, want %q", got, savedClosed)
	}
	r.pressJabra(t)
	r.requireState(t, micRecording, "a Jabra press after a page pause")
	<-r.power.press(nil, nil)
	r.requireState(t, micClosed, "a power-button pause with the listener off")
	r.saidPhrase()
	r.requireState(t, micClosed, "the phrase, with the listener off")
}

// TestNoJabraButtonEveryPauseCloses fails if a pause goes to listening while
// no Jabra mute button works: nothing in the room could then close the
// microphone.
func TestNoJabraButtonEveryPauseCloses(t *testing.T) {
	r := startMicStatesRig(t, statesOptions{listener: true, jabra: false})
	if err := r.hooks.Pause(); err != nil {
		t.Fatal(err)
	}
	r.requireState(t, micClosed, "a page pause with no Jabra button")
	if err := r.hooks.Resume(); err != nil {
		t.Fatal(err)
	}
	r.requireState(t, micRecording, "the resume")
	<-r.power.press(nil, nil)
	r.requireState(t, micClosed, "a power-button pause with no Jabra button")
}

// TestJabraDetachInListeningCloses fails if a unit that listens keeps
// listening once its Jabra's mute button detaches, or opens again when it
// comes back.
func TestJabraDetachInListeningCloses(t *testing.T) {
	r := startMicStatesRig(t, statesOptions{listener: true, jabra: true})
	r.moveTo(t, micListening)
	r.jabra.unplug()
	waitForStates(t, "the Jabra to detach", func() bool { return !r.device.jabraAttached() })
	r.jabra = nil // gone: the ring is what it must show, ringMuted
	r.requireState(t, micClosed, "the Jabra's detach while listening")
	if got := r.saved(); got != savedClosed {
		t.Fatalf("saved %q after the detach, want %q", got, savedClosed)
	}
	// It comes back: the unit stays closed, and its ring shows Muted.
	r.jabra = newJabraFake()
	r.jabras <- r.jabra
	waitForStates(t, "the Jabra to attach again", r.device.jabraAttached)
	time.Sleep(100 * time.Millisecond)
	r.requireState(t, micClosed, "the Jabra attaching again after a detach while listening")
}

// TestCloseFromEveryState fails if POST /api/close leaves any state but
// closed, or saves any other state.
func TestCloseFromEveryState(t *testing.T) {
	for _, listener := range []bool{true, false} {
		for _, from := range []micState{micRecording, micListening, micClosed} {
			if from == micListening && !listener {
				continue
			}
			t.Run(fmt.Sprintf("%v, listener %t", from, listener), func(t *testing.T) {
				r := startMicStatesRig(t, statesOptions{listener: listener, jabra: true})
				r.moveTo(t, from)
				if err := r.hooks.Close(); err != nil {
					t.Fatal(err)
				}
				r.requireState(t, micClosed, "POST /api/close from "+from.String())
				if got := r.saved(); got != savedClosed {
					t.Fatalf("saved %q, want %q", got, savedClosed)
				}
			})
		}
	}
}

// TestRefusedResumeReturnsToSavedState fails if a resume the light refuses
// leaves the unit in any state but the one it came from, or saves another:
// a refusal from listening must not close the microphone, and one from
// closed must not open it.
func TestRefusedResumeReturnsToSavedState(t *testing.T) {
	for _, from := range []micState{micListening, micClosed} {
		for _, how := range []string{"the page's resume", "the phrase"} {
			if how == "the phrase" && from == micClosed {
				continue // nothing hears it there
			}
			t.Run(fmt.Sprintf("%v, %s", from, how), func(t *testing.T) {
				r := startMicStatesRig(t, statesOptions{listener: true, jabra: true})
				r.moveTo(t, from)
				r.light.refuse.Store(true)
				if how == "the phrase" {
					r.saidPhrase()
				} else if err := r.hooks.Resume(); err == nil {
					t.Fatal("control: the resume was not refused with the light reading off")
				}
				r.requireState(t, from, "a refused resume from "+from.String())
				if got := r.saved(); got != savedValue(from) {
					t.Fatalf("saved %q after the refused resume, want %q", got, savedValue(from))
				}
			})
		}
	}
}

// TestSavedStateRestores fails if a saved pause opens the room's microphone
// with no person's choice: a pause saved before the listener ("1") comes
// back closed; a saved listening comes back closed with the listener off,
// and at boot until the Jabra's button attaches, and listening only then.
func TestSavedStateRestores(t *testing.T) {
	t.Run("a pause saved before the listener", func(t *testing.T) {
		r := startMicStatesRig(t, statesOptions{listener: true, jabra: true, saved: "1"})
		r.requireState(t, micClosed, "restoring a saved \"1\"")
		time.Sleep(100 * time.Millisecond)
		r.requireState(t, micClosed, "100 ms after restoring a saved \"1\"")
	})
	t.Run("listening, with the listener off", func(t *testing.T) {
		r := startMicStatesRig(t, statesOptions{listener: false, jabra: true, saved: savedListening})
		r.requireState(t, micClosed, "restoring a saved listening with the listener off")
		time.Sleep(100 * time.Millisecond)
		r.requireState(t, micClosed, "100 ms after restoring a saved listening with the listener off")
	})
	t.Run("listening, before the Jabra attaches", func(t *testing.T) {
		r := startMicStatesRig(t, statesOptions{listener: true, jabra: false, saved: savedListening})
		time.Sleep(100 * time.Millisecond)
		r.requireState(t, micClosed, "restoring a saved listening before the Jabra's button attaches")
		// The button attaches.
		r.jabra = newJabraFake()
		button := newJabraMuteButton(r.capture, r.device, r.hooks, r.warnings)
		var connected atomic.Bool
		button.connect = func() (io.ReadWriteCloser, error) {
			if connected.Swap(true) {
				return nil, jabra.ErrNoMuteButton
			}
			return r.jabra, nil
		}
		button.retryEvery, button.checkEvery = time.Millisecond, time.Millisecond
		ctx, cancel := context.WithCancel(context.Background())
		done := goDone(func() { button.run(ctx) })
		t.Cleanup(func() { cancel(); <-done })
		r.requireState(t, micListening, "the Jabra's button attaching to a saved listening")
	})
}
