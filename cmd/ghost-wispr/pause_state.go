package main

import (
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

type pauseStore interface {
	GetAppState(key string) (string, bool, error)
	SetAppState(key, value string) error
}

// restorePause applies the last persisted capture switch before any microphone
// capture begins. It returns true when the switch was paused.
func restorePause(store pauseStore, gate interface{ Pause() }) (bool, error) {
	value, ok, err := store.GetAppState(storage.AppStateCapturePaused)
	if err != nil {
		return false, fmt.Errorf("read persisted capture pause: %w", err)
	}
	if !ok || value == "0" {
		return false, nil
	}
	if value != "1" {
		return false, fmt.Errorf("invalid persisted capture pause value %q", value)
	}
	gate.Pause()
	return true, nil
}

// wirePauseControls serves the API's Pause and Resume with controls, and the
// light's consent check with them: each keeps the warning that the saved
// pause switch is not what a restart would restore (captureStateWarningKey)
// while that is so.
func wirePauseControls(hooks *server.ControlHooks, controls *persistentControls, capture *captureGate, hardware *hardwareControl, warnings *warningsRegistry) {
	// pauseNotSaved warns that the durable pause switch could not be saved.
	pauseNotSaved := func() {
		warnings.Set(captureStateWarningKey, "Pause not saved; a restart will resume recording")
	}
	hooks.Pause = func() error {
		err := controls.Pause()
		if err != nil {
			pauseNotSaved()
		} else {
			warnings.Clear(captureStateWarningKey)
		}
		return err
	}
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
	if err := store.SetAppState(storage.AppStateCapturePaused, "1"); err != nil {
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
	resume          func() error
	onStatusChanged func(bool)
	mu              sync.Mutex
}

var errRefusedResumeNotPersisted = errors.New("refused resume pause state was not persisted")

func newPersistentControls(store pauseStore, pause func(), resume func() error, onStatusChanged func(bool)) *persistentControls {
	return &persistentControls{store: store, pause: pause, resume: resume, onStatusChanged: onStatusChanged}
}

func (c *persistentControls) Pause() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pause()
	err := c.store.SetAppState(storage.AppStateCapturePaused, "1")
	if c.onStatusChanged != nil {
		c.onStatusChanged(true)
	}
	if err != nil {
		return fmt.Errorf("persist capture pause: %w", err)
	}
	return nil
}

func (c *persistentControls) Resume() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.store.SetAppState(storage.AppStateCapturePaused, "0"); err != nil {
		return fmt.Errorf("persist capture resume: %w", err)
	}
	if err := c.resume(); err != nil {
		if storeErr := c.store.SetAppState(storage.AppStateCapturePaused, "1"); storeErr != nil {
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

func (c *persistentControls) RefuseResume() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	err := c.store.SetAppState(storage.AppStateCapturePaused, "1")
	if c.onStatusChanged != nil {
		c.onStatusChanged(true)
	}
	if err != nil {
		return fmt.Errorf("persist refused capture resume: %w", err)
	}
	return nil
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

// startMicIfUnpaused starts the opened microphone unless capture is paused. A
// paused instance opens no device (startMicrophone), but a pause that lands
// after the device opened leaves it open and not started; the mic loop starts
// it on resume. Each start attempt confirms red first and clears any
// forced-red hold afterward.
func startMicIfUnpaused(capture *captureGate, start func() error) error {
	if capture.IsPaused() {
		return nil
	}
	if err := capture.confirmMicStart(); err != nil {
		return fmt.Errorf("%w: %w", errLightNotConfirmed, err)
	}
	defer capture.finishMicStart()
	return start()
}

// startOpenedMic starts mic, which the caller has opened and published,
// unless capture is paused (startMicIfUnpaused). If the start fails, because
// the light could not be confirmed red or the device did not start, it
// withdraws mic with withdraw, then closes it, so a failed start holds no
// input device. A start skipped for a pause keeps the device open and not
// started, for the mic loop to start on resume.
func startOpenedMic(capture *captureGate, mic interface {
	Start() error
	Close() error
}, withdraw func(), logf func(string, ...any)) error {
	if err := startMicIfUnpaused(capture, mic.Start); err != nil {
		withdraw()
		if closeErr := mic.Close(); closeErr != nil {
			logf("warning: close microphone after its start failed: %v", closeErr)
		}
		return err
	}
	return nil
}
