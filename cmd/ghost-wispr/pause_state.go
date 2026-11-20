package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

type pauseStore interface {
	GetAppState(key string) (string, bool, error)
	SetAppState(key, value string) error
}

// The saved state's values (storage.AppStateCapturePaused). "1" is today's
// pause, and every pause saved before the paused listener: it is closed, so
// the first deploy of the listener never opens the room's microphone.
const (
	savedRecording = "0"
	savedClosed    = "1"
	savedListening = "2"
)

// savedValue is what the store holds for state.
func savedValue(state micState) string {
	switch state {
	case micRecording:
		return savedRecording
	case micListening:
		return savedListening
	default:
		return savedClosed
	}
}

// restorePause applies the last persisted capture state before any
// microphone capture begins, and returns that state. A saved pause, of either
// level, restores as closed: a saved listening comes back closed until the
// Jabra button is attached and the listener has started, which the controls
// see to (restoreListening), and with no listener it stays closed.
func restorePause(store pauseStore, gate interface{ Pause() }) (micState, error) {
	value, ok, err := store.GetAppState(storage.AppStateCapturePaused)
	if err != nil {
		return micRecording, fmt.Errorf("read persisted capture pause: %w", err)
	}
	switch {
	case !ok || value == savedRecording:
		return micRecording, nil
	case value == savedClosed:
		gate.Pause()
		return micClosed, nil
	case value == savedListening:
		gate.Pause()
		return micListening, nil
	default:
		return micRecording, fmt.Errorf("invalid persisted capture pause value %q", value)
	}
}

// wirePauseControls serves the API's Pause, Close and Resume with controls,
// and the light's consent check with them: each keeps the warning that the
// saved pause switch is not what a restart would restore
// (captureStateWarningKey) while that is so.
func wirePauseControls(hooks *server.ControlHooks, controls *persistentControls, capture *captureGate, hardware *hardwareControl, warnings *warningsRegistry) {
	// pauseNotSaved warns that the durable pause switch could not be saved.
	pauseNotSaved := func() {
		warnings.Set(captureStateWarningKey, "Pause not saved; a restart will resume recording")
	}
	saved := func(err error) error {
		if err != nil {
			pauseNotSaved()
		} else {
			warnings.Clear(captureStateWarningKey)
		}
		return err
	}
	hooks.Pause = func() error { return saved(controls.Pause()) }
	hooks.Close = func() error { return saved(controls.Close()) }
	hooks.Resume = func() error {
		err := controls.Resume()
		if err != nil {
			if errors.Is(err, errRefusedResumeNotPersisted) {
				pauseNotSaved()
			}
		} else {
			warnings.Clear(captureStateWarningKey)
		}
		return err
	}
	wireLightConsent(capture, hardware, warnings, func(error) {
		if err := controls.RefuseResume(); err != nil {
			pauseNotSaved()
		}
	})
}

// seedPausedState writes the durable pause before a test instance starts, so
// its microphone never starts without a recording light. It resolves the
// configuration and database exactly as the recorder does, prints the
// absolute database path it wrote, and returns that path.
func seedPausedState(configPath string, out io.Writer) (string, error) {
	cfgStore, _, err := loadRuntimeConfig(configPath)
	if err != nil {
		return "", fmt.Errorf("load config: %w", err)
	}
	dbPath, err := databasePath(cfgStore.Get().DBPath)
	if err != nil {
		return "", err
	}
	store, err := storage.NewSQLiteStore(dbPath)
	if err != nil {
		return "", fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = store.Close() }()
	if err := store.SetAppState(storage.AppStateCapturePaused, savedClosed); err != nil {
		return "", fmt.Errorf("set capture pause: %w", err)
	}
	if _, err := fmt.Fprintf(out, "seed-paused-state: capture pause written to %s\n", dbPath); err != nil {
		return "", fmt.Errorf("report database path: %w", err)
	}
	return dbPath, nil
}

// persistentControls serializes pause and resume state transitions with the
// durable switch they publish.
type persistentControls struct {
	store           pauseStore
	pause           func()
	gate            resumeGate
	onStatusChanged func(bool)
	mu              sync.Mutex
	// The three-state machine's parts (useListener); without them every
	// pause closes the microphone, and Close is Pause: today's two-state
	// machine, as with voice.paused_listener off or a build with no listener.
	levels micLevels
	// wake wakes the recording light after a pause or a close.
	wake func()
	// ready reports whether the paused listener can run: it is on, built and
	// started, and the Jabra's mute button, which closes the microphone, is
	// attached. A pause goes to listening only while it can.
	ready func() bool
	// stopListener closes the listener's stream, and waits until it has,
	// before a resume's light check: the recording microphone opens only
	// after the listener's has closed.
	stopListener func()
	// resumes counts the resumes under way; the listener does not listen
	// while one runs.
	resumes atomic.Int32
	// restoreListening is set while a saved listening state waits, closed,
	// for the listener to be ready; any control clears it.
	restoreListening bool
}

// micLevels is the capture gate's side of the three states.
type micLevels interface {
	micState() micState
	PauseListening()
	CloseMicrophone()
	leaveListening() bool
	enterListening() bool
}

// resumeGate is the capture gate's resume in its two steps (captureGate):
// prepareResume, the light's check, which can take seconds, and resumeWith,
// the resume itself, which a Pause called since the check overtakes.
// pauseRequested counts a pause the moment a person asks for it, before the
// controls' lock, and IsPaused is the gate's switch.
type resumeGate interface {
	prepareResume() (resumeTicket, error)
	resumeWith(t resumeTicket, persist func() error) error
	pauseRequested()
	IsPaused() bool
}

var errRefusedResumeNotPersisted = errors.New("refused resume pause state was not persisted")

func newPersistentControls(store pauseStore, pause func(), gate resumeGate, onStatusChanged func(bool)) *persistentControls {
	return &persistentControls{store: store, pause: pause, gate: gate, onStatusChanged: onStatusChanged}
}

// useListener gives the controls the three states: levels moves the gate
// between them, wake wakes the light after each pause or close, ready says
// whether the listener can run, and stopListener closes its stream.
func (c *persistentControls) useListener(levels micLevels, wake func(), ready func() bool, stopListener func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.levels, c.wake, c.ready, c.stopListener = levels, wake, ready, stopListener
}

// state reports the microphone's state.
func (c *persistentControls) state() micState {
	if c.levels != nil {
		return c.levels.micState()
	}
	if c.gate.IsPaused() {
		return micClosed
	}
	return micRecording
}

// canListen reports whether a pause may go to listening.
func (c *persistentControls) canListen() bool {
	return c.levels != nil && c.ready != nil && c.ready()
}

// Pause counts the pause on the gate before it takes the controls' lock,
// which another resume's save may hold: a resume called after this pause then
// carries it and is not overtaken by it, and a resume whose light check it
// came during is overtaken (review 5963682292 of #79). The Pause it then
// makes takes that count rather than counting again. A unit that records goes
// to listening while the listener can run, and to closed otherwise; a paused
// unit stays as it is.
func (c *persistentControls) Pause() error {
	c.gate.pauseRequested()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.canListen() {
		c.levels.PauseListening()
		c.wake()
	} else {
		c.pause()
	}
	return c.savePause("persist capture pause")
}

// Close moves every state to closed: POST /api/close, the page's "Close
// microphone", and the Jabra's mute button. It counts as a pause, so it
// overtakes a resume whose light check runs.
func (c *persistentControls) Close() error {
	c.gate.pauseRequested()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.levels != nil {
		c.levels.CloseMicrophone()
		c.wake()
	} else {
		c.pause()
	}
	return c.savePause("persist microphone close")
}

// savePause saves the paused state the unit is in, clears a saved listening
// state's wait (a person acted), and tells the clients. The caller holds mu.
func (c *persistentControls) savePause(what string) error {
	c.restoreListening = false
	err := c.store.SetAppState(storage.AppStateCapturePaused, savedValue(c.state()))
	if c.onStatusChanged != nil {
		c.onStatusChanged(true)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return nil
}

// Resume closes the listener's stream first, and keeps the listener from
// listening until it returns. It runs the light's check before it takes the
// controls' lock, so a person's Pause meanwhile is not held up behind it, and
// that Pause, the later call, wins: the resume then saves nothing and returns
// nil (review 5961636823 of #79). The resume saves the switch on just before
// it confirms the light red, and, if the light refuses it, saves the state it
// came from again: the unit goes back to it.
func (c *persistentControls) Resume() error {
	c.resumes.Add(1)
	defer c.resumes.Add(-1)
	c.mu.Lock()
	stop := c.stopListener
	c.mu.Unlock()
	if stop != nil {
		stop()
	}
	ticket, err := c.gate.prepareResume()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.restoreListening = false
	if err != nil {
		// Nothing changed. The clients hear the gate's switch as it is: a
		// resume that overtook this one may have moved it on, and the unit
		// then records (review 5963682292 of #79).
		paused := c.gate.IsPaused()
		if c.onStatusChanged != nil {
			c.onStatusChanged(paused)
		}
		if !paused {
			return nil
		}
		return err
	}
	var persistErr error
	err = c.gate.resumeWith(ticket, func() error {
		persistErr = c.store.SetAppState(storage.AppStateCapturePaused, savedRecording)
		return persistErr
	})
	switch {
	case persistErr != nil:
		return fmt.Errorf("persist capture resume: %w", persistErr)
	case errors.Is(err, errResumeOvertaken):
		return nil // the later pause saved and announced the switch off
	case err != nil:
		if storeErr := c.store.SetAppState(storage.AppStateCapturePaused, savedValue(c.state())); storeErr != nil {
			if c.onStatusChanged != nil {
				c.onStatusChanged(true)
			}
			return fmt.Errorf("%w: %w", errRefusedResumeNotPersisted, errors.Join(err, storeErr))
		}
		if c.onStatusChanged != nil {
			c.onStatusChanged(true)
		}
		return err
	}
	if c.onStatusChanged != nil {
		c.onStatusChanged(false)
	}
	return nil
}

// RefuseResume saves the state a resume the light refused at the gate's
// opening returned the unit to: the one it came from.
func (c *persistentControls) RefuseResume() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	err := c.store.SetAppState(storage.AppStateCapturePaused, savedValue(c.state()))
	if c.onStatusChanged != nil {
		c.onStatusChanged(true)
	}
	if err != nil {
		return fmt.Errorf("persist refused capture resume: %w", err)
	}
	return nil
}

// listenerWanted reports whether the paused listener should hold the
// microphone now: the unit is listening, the listener can run, and no resume
// is under way.
func (c *persistentControls) listenerWanted() bool {
	c.mu.Lock()
	levels, ready := c.levels, c.ready
	c.mu.Unlock()
	return levels != nil && ready != nil && levels.micState() == micListening && c.resumes.Load() == 0 && ready()
}

// armRestoredListening makes a saved listening state, restored as closed,
// come back once the listener is ready (listenerChanged).
func (c *persistentControls) armRestoredListening() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.restoreListening = true
}

// listenerChanged follows a change in whether the listener can run: a unit
// that listens goes to closed once it cannot, as when the Jabra's button
// detaches or the listener is turned off, and a saved listening state that
// waits comes back once it can.
func (c *persistentControls) listenerChanged() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.levels == nil {
		return nil
	}
	ready := c.canListen()
	switch {
	case !ready && c.levels.leaveListening():
		c.wake()
		return c.savePause("persist the microphone closed for the listener")
	case ready && c.restoreListening && c.levels.micState() == micClosed:
		c.restoreListening = false
		c.levels.enterListening()
		c.wake()
		if c.onStatusChanged != nil {
			c.onStatusChanged(true)
		}
	}
	return nil
}

// listenerFailed moves a listening unit to closed: its listener's capture
// failed, so nothing listens, and the ring must show that the microphone is
// closed.
func (c *persistentControls) listenerFailed() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.levels == nil || !c.levels.leaveListening() {
		return nil
	}
	c.wake()
	return c.savePause("persist the microphone closed for the listener")
}

// micStartFailureMessages returns the microphone warning and the start-up
// status message for a failed microphone start. A start the recording light
// refused adds no microphone warning: the light's own warning names the cause.
func micStartFailureMessages(err error) (warning, status string) {
	if errors.Is(err, errLightNotConfirmed) {
		return "", "Recording light not confirmed red; the microphone does not start until it is"
	}
	return "Microphone failed to start — recording and live transcription are disabled", "Microphone failed to start"
}

// errLightNotConfirmed tags a microphone start refused because the recording
// light could not be confirmed red; the light's own warning names the cause.
var errLightNotConfirmed = errors.New("recording light not confirmed red")

// errNoCaptureDevice is what a microphone start or reconnect returns, without
// lighting the light or opening anything, while no capture device is present:
// the light stays off while the microphone is missing.
var errNoCaptureDevice = errors.New("no capture device present")

// errMicStartPaused is what a microphone start returns when a pause came
// before it opened a device: it opened nothing, and the next resume starts
// the microphone.
var errMicStartPaused = errors.New("microphone start cancelled by a pause")

// micStartCancelled reports why a microphone start must open no device now:
// errMicStartPaused once a pause came during its scan, and an error that
// wraps ctx's once shutdown has begun. Shutdown waits a bounded time for the
// mic loops and then turns the light off, so a microphone opened after it
// would be open with the light off, and nothing would close it.
func micStartCancelled(ctx context.Context, capture *captureGate) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("microphone start cancelled by shutdown: %w", err)
	}
	if capture.isClosed() {
		return errMicStartPaused
	}
	return nil
}

// startMicIfUnpaused starts the opened microphone unless capture is paused. A
// paused instance opens no device (startMicrophone), but a pause that lands
// after the device opened leaves it open and not started; the mic loop closes
// it, and opens it again on resume. Each start attempt confirms red first and
// holds it red until the start returns (openUnderRed).
func startMicIfUnpaused(capture *captureGate, start func() error) error {
	if capture.IsPaused() {
		return nil
	}
	return capture.openUnderRed(start)
}

// startOpenedMic starts mic, which the caller has opened and published,
// unless capture is paused (startMicIfUnpaused). If the start fails, because
// the light could not be confirmed red or the device did not start, it
// withdraws mic with withdraw, then closes it, so a failed start holds no
// input device. A start skipped for a pause leaves the device open and not
// started, for the mic loop to close.
func startOpenedMic(capture *captureGate, mic interface {
	Start() error
	Close() error
}, withdraw func()) error {
	if err := startMicIfUnpaused(capture, mic.Start); err != nil {
		withdraw()
		if closeErr := mic.Close(); closeErr != nil {
			slog.Warn("close microphone after its start failed", "error", closeErr)
		}
		return err
	}
	return nil
}
