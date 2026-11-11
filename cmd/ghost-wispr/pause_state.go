package main

import (
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

// persistingControls makes a pause durable after capture has stopped and makes
// a resume durable before capture can begin. A failed store write therefore
// keeps capture stopped.
func persistingControls(store pauseStore, pause, resume func()) (p, r func() error) {
	var mu sync.Mutex

	p = func() error {
		mu.Lock()
		defer mu.Unlock()
		pause()
		if err := store.SetAppState(storage.AppStateCapturePaused, "1"); err != nil {
			return fmt.Errorf("persist capture pause: %w", err)
		}
		return nil
	}
	r = func() error {
		mu.Lock()
		defer mu.Unlock()
		if err := store.SetAppState(storage.AppStateCapturePaused, "0"); err != nil {
			return fmt.Errorf("persist capture resume: %w", err)
		}
		resume()
		return nil
	}
	return p, r
}

// startMicIfUnpaused leaves a restored pause with an open device handle but
// without starting microphone capture. The mic loop starts it on resume.
func startMicIfUnpaused(capture *captureGate, start func() error) error {
	if capture.IsPaused() {
		return nil
	}
	return start()
}
