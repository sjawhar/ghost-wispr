package main

import (
	"errors"
	"fmt"
	"io"
	"sync"

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
		return err
	}
	defer capture.finishMicStart()
	return start()
}
