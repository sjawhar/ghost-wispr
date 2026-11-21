package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/wake/local"
)

// TestTheButtonsAndTheListenerRunAsWorkTheStopWaitsFor fails if the power
// button, the Jabra's mute button or the paused listener runs outside the
// work the stop waits for (backgroundWork): each saves the pause state on a
// press or a phrase, so the store must not close under it. Each is started as
// main starts it; with no device each looks for its device again, inside the
// work, until its context ends.
func TestTheButtonsAndTheListenerRunAsWorkTheStopWaitsFor(t *testing.T) {
	capture := newCaptureGate(nil, nil, nil)
	for _, tc := range []struct {
		name  string
		start func(ctx context.Context, bg *watchedBackground) (inside func() bool)
	}{
		{"power button", func(ctx context.Context, bg *watchedBackground) func() bool {
			startPowerButton(ctx, &config.Config{PowerButton: true}, capture, &server.ControlHooks{}, newWarningsRegistry(), bg)
			return bg.inside
		}},
		{"Jabra mute button", func(ctx context.Context, bg *watchedBackground) func() bool {
			startJabraMuteButton(ctx, &config.Config{JabraMuteButton: true}, capture, newCaptureDevice(capture.inClosedState), &server.ControlHooks{}, newWarningsRegistry(), func() {}, bg)
			return bg.inside
		}},
		{"paused listener", func(ctx context.Context, bg *watchedBackground) func() bool {
			cfgStore := appLogConfigStore(t)
			if err := cfgStore.Update(func(c *config.Config) { c.Voice.Enabled, c.Voice.PausedListener = true, true }); err != nil {
				t.Fatal(err)
			}
			var loadedInside atomic.Bool // the spotter loaded inside the work the stop waits for
			persistent := newPersistentControls(&pauseStoreFake{}, capture.Pause, capture, nil)
			startPausedListener(ctx, cfgStore, &pausedListenerUnit{
				capture: capture, controls: persistent, hooks: &server.ControlHooks{}, device: newCaptureDevice(capture.inClosedState),
				wake: func() {}, warnings: newWarningsRegistry(), built: true, background: bg,
				newDetector: func() (*local.Detector, error) {
					loadedInside.Store(bg.inside())
					return nil, errors.New("no spotter in this test")
				},
			})
			return loadedInside.Load
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bg := &watchedBackground{}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			inside := tc.start(ctx, bg)
			deadline := time.Now().Add(5 * time.Second)
			for !inside() {
				if time.Now().After(deadline) {
					t.Fatalf("the %s does not run as work the stop waits for (admitted %d)", tc.name, bg.admitted.Load())
				}
				time.Sleep(5 * time.Millisecond)
			}
			cancel()
			for bg.inside() {
				if time.Now().After(deadline) {
					t.Fatalf("the %s did not end with its context", tc.name)
				}
				time.Sleep(5 * time.Millisecond)
			}
		})
	}
}
