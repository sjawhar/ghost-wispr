package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/session"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/storeguard"
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
			loaded := make(chan bool, 1)
			persistent := newPersistentControls(&pauseStoreFake{}, capture.Pause, capture, nil)
			startPausedListener(ctx, cfgStore, &pausedListenerUnit{
				capture: capture, controls: persistent, hooks: &server.ControlHooks{}, device: newCaptureDevice(capture.inClosedState),
				wake: func() {}, warnings: newWarningsRegistry(), built: true, background: bg,
				newDetector: func() (*local.Detector, error) {
					loaded <- bg.inside()
					return nil, errors.New("no spotter in this test")
				},
			})
			return func() bool {
				select {
				case in := <-loaded:
					return in
				case <-ctx.Done():
					return false
				}
			}
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

// TestTheStopWaitsForAResumesGateOpening fails if the stop returns while the
// gate's opening after a resume still runs: a light that is not red refuses
// the resume there, which saves the pause state (onResumeRefused), after the
// store closed (storeguard records the write).
func TestTheStopWaitsForAResumesGateOpening(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "gate.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	recorder := audio.NewRecorder(t.TempDir())
	manager := session.NewManager(store, recorder, nil, nil, session.NewDetector(time.Hour), 0)
	capture := newSessionCaptureGate(context.Background(), manager, recorder, func() deepgramFinalizer { return nil }, time.Hour, testEndRetry, nopLogf)
	capture.background = manager
	opening := make(chan struct{})
	capture.beforeGateOpen = func() error {
		close(opening)
		time.Sleep(200 * time.Millisecond) // reading the light back
		return errors.New("the recording light does not show red")
	}
	refused := make(chan struct{})
	capture.onResumeRefused = func(error) {
		defer close(refused)
		_ = store.SetAppState(storage.AppStateCapturePaused, "true")
	}
	guard := storeguard.New(t, store)
	capture.Pause()
	if err := capture.RequestResume(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-opening:
	case <-time.After(5 * time.Second):
		t.Fatal("control: the resume did not reach the gate's opening")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := manager.StopBackground(ctx); err != nil {
		t.Fatalf("the stop did not wait for the gate's opening: %v", err)
	}
	guard.Close(t)
	select {
	case <-refused:
	case <-time.After(5 * time.Second):
		t.Fatal("control: the light did not refuse the resume")
	}
	time.Sleep(50 * time.Millisecond)
	guard.Check(t)
}
