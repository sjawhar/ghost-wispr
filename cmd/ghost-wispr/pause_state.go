package main

import (
	"errors"
	"fmt"
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

// startMicIfUnpaused leaves a restored pause with an open device handle but
// without starting microphone capture. The mic loop starts it on resume. Each
// start attempt confirms red first and clears any forced-red hold afterward.
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
