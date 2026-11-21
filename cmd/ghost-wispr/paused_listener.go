package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"

	"golang.org/x/sys/unix"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/wake/local"
)

const pausedListenerWarningKey = "paused-listener"

// pausedListenerUnit is what the paused listener needs from the rest of the
// unit, as main wires it.
type pausedListenerUnit struct {
	capture  *captureGate
	controls *persistentControls
	hooks    *server.ControlHooks
	device   *captureDevice
	wake     func()
	warnings *warningsRegistry
	// restored is the state the unit saved before it last stopped
	// (restorePause).
	restored micState
	// built is local.Built; tests set it.
	built bool
	// newDetector loads the spotter; tests replace it.
	newDetector func() (*local.Detector, error)
	// openCapture opens the ALSA capture; tests replace it.
	openCapture func() (local.Capture, error)
	// background runs the listener as work the stop waits for: a phrase it
	// hears resumes, which saves the pause state. main sets it to the session
	// manager; nil runs the listener on a goroutine of its own.
	background backgroundWork
}

// listenerOn reports whether the settings turn the paused listener on: both
// voice.paused_listener and voice.enabled, since with the voice off Ghost
// Wispr runs no wake detection (INFRA-4592 R13).
func listenerOn(cfg *config.Config) bool { return cfg.Voice.Enabled && cfg.Voice.PausedListener }

// startPausedListener gives the unit its first pause level, listening, in a
// build that has the paused listener (INFRA-4592 R1, R2). A pause goes to
// listening only while the settings turn it on (listenerOn), the listener has
// started and the Jabra's mute button is attached; otherwise, and always in a
// build without the listener, the unit is today's two-state machine: every
// pause closes the microphone. A saved listening comes back closed until the
// listener can run, and a listening unit goes to closed when it no longer can:
// the button detaches, a setting turns off, or the listener's capture fails.
// The listener is an ALSA capture in this process, opened and closed through
// the capture device, which hands the microphone to and from the recording
// microphone.
func startPausedListener(ctx context.Context, cfgStore *config.Store, u *pausedListenerUnit) {
	cfg := cfgStore.Get()
	if !u.built {
		if listenerOn(&cfg) {
			slog.Warn("voice.paused_listener is on, but this build has no paused listener; every pause closes the microphone")
			u.warnings.Set(pausedListenerWarningKey, "voice.paused_listener is on, but this build has no paused listener: every pause closes the microphone")
		}
		return
	}
	if u.newDetector == nil {
		threads := cfg.Voice.PausedListenerThreads
		u.newDetector = func() (*local.Detector, error) {
			return local.NewDetectorWith(&local.Options{Thresholds: local.DefaultThresholds, Threads: threads})
		}
	}
	if u.openCapture == nil {
		name := audio.CaptureDevice(cfg.MicDevice)
		u.openCapture = func() (local.Capture, error) { return audio.OpenALSACapture(name, local.SampleRate) }
	}
	var configured, started atomic.Bool
	configured.Store(listenerOn(&cfg))
	ready := func() bool { return configured.Load() && started.Load() && u.device.jabraAttached() }
	u.controls.useListener(u.capture, u.wake, ready, u.device.closeListener)
	if u.restored == micListening && listenerOn(&cfg) {
		u.controls.armRestoredListening()
	}
	changed := func() {
		if err := u.controls.listenerChanged(); err != nil {
			slog.Warn("paused listener", "error", err)
		}
	}
	u.device.onAttachChanged(changed)
	var start sync.Once
	startListener := func() {
		start.Do(func() {
			goUntilStop(ctx, u.background, func(ctx context.Context) { runPausedListener(ctx, u, &started, changed) })
		})
	}
	onConfigChange(cfgStore, func(cfg *config.Config) {
		on := listenerOn(cfg)
		if configured.Swap(on) != on {
			changed()
		}
		if on {
			startListener()
		}
	})
	if listenerOn(&cfg) {
		startListener()
	}
}

// runPausedListener loads the spotter, then listens until ctx ends. The
// model takes about a second to load, so main does not wait for it; until it
// has, started is false and every pause closes the microphone.
func runPausedListener(ctx context.Context, u *pausedListenerUnit, started *atomic.Bool, changed func()) {
	// The listener's ring holds up to 2 s of the room's audio in memory; a
	// core dump would write it to disk. The service unit also sets
	// LimitCORE=0 and MemorySwapMax=0.
	if err := unix.Setrlimit(unix.RLIMIT_CORE, &unix.Rlimit{Cur: 0, Max: 0}); err != nil {
		slog.Warn("paused listener: core dumps not turned off; the listener does not start", "error", err)
		u.warnings.Set(pausedListenerWarningKey, "Paused listener off: core dumps could not be turned off; every pause closes the microphone")
		return
	}
	detector, err := u.newDetector()
	if err != nil {
		slog.Warn("paused listener: the spotter did not load; every pause closes the microphone", "error", err)
		u.warnings.Set(pausedListenerWarningKey, "Paused listener off: its spotter did not load; every pause closes the microphone")
		return
	}
	defer detector.Close()
	listener := local.NewListener(detector, local.ListenerConfig{
		Open: func(stretch uint64) (local.Capture, error) {
			var capture local.Capture
			inStretch := func() bool {
				current, wanted := u.controls.listenerWanted()
				return wanted && current == stretch
			}
			err := u.device.openListener(inStretch, func() error {
				var err error
				capture, err = u.openCapture()
				return err
			})
			if err != nil {
				return nil, err
			}
			u.warnings.Clear(pausedListenerWarningKey)
			return listenerStream{capture: capture, device: u.device}, nil
		},
		Want: u.controls.listenerWanted,
		// The phrase resumes as POST /api/resume does, so the light is
		// confirmed red before any audio passes the gate, but only while the
		// unit still listens in the stretch the phrase was heard in.
		Heard: func(stretch uint64) {
			err := noteResume(u.warnings, u.controls.resumeFromListening(stretch))
			switch {
			case errors.Is(err, errStaleListening):
				slog.Info("paused listener: the phrase came from a stretch of listening that has ended; nothing resumed")
				return
			case err != nil:
				slog.Warn("paused listener: resume refused", "error", err)
				return
			}
			if u.hooks.OnStatusChanged != nil {
				u.hooks.OnStatusChanged(false)
			}
		},
		Failed: func(stretch uint64, _ error) {
			moved, err := u.controls.listenerFailed(stretch)
			if moved {
				u.warnings.Set(pausedListenerWarningKey, "Paused listener stopped: its microphone did not open or failed; the microphone is closed")
			}
			if err != nil {
				slog.Warn("paused listener", "error", err)
			}
		},
	})
	u.device.setListener(listener.Interrupt)
	started.Store(true)
	changed()
	listener.Run(ctx)
}

// listenerStream is the listener's capture, whose close the capture device
// counts, so a recording microphone waiting in closeListener goes on.
type listenerStream struct {
	capture local.Capture
	device  *captureDevice
}

func (s listenerStream) Read(pcm []int16) (int, error) { return s.capture.Read(pcm) }

func (s listenerStream) Close() error { return s.device.closeListenerStream(s.capture.Close) }
