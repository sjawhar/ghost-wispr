package main

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// speechResumeRig is a paused unit with a Jabra whose ring the test reads, its
// capture device wired to the gate as main wires it, and speech playing on the
// Jabra.
type speechResumeRig struct {
	capture *captureGate
	device  *captureDevice
	ring    atomic.Bool // the Jabra's mute light as last written: true shows Muted
	reds    atomic.Int32
	out     *heldOutput
	finish  func()
}

func newSpeechResumeRig(t *testing.T) *speechResumeRig {
	t.Helper()
	r := &speechResumeRig{capture: newCaptureGate(nil, nil, nil)}
	r.capture.Pause()
	r.capture.stallAfter = 30 * time.Millisecond
	r.capture.beforeMicStart = func() error { r.reds.Add(1); return nil }
	r.device = newCaptureDevice(r.capture.IsPaused)
	r.capture.beforeMicOpen = r.device.prepareLightForOpen
	detach := r.device.attach(func(on bool) error { r.ring.Store(on); return nil })
	t.Cleanup(detach)
	if !r.ring.Load() {
		t.Fatal("control: the Jabra's ring does not show the pause")
	}
	return r
}

// startMicLoop runs the mic loop on a scripted microphone through the rig's
// capture device, once the paused loop has closed the microphone.
func (r *speechResumeRig) startMicLoop(t *testing.T) *scriptedMic {
	t.Helper()
	mic := newScriptedMic()
	runMicLoopOn(t, r.capture, orderedMic{micStreamer: mic, device: r.device}, mic, (&recordedWaits{}).wait)
	mic.expectEvents(t, "close")
	return mic
}

func (r *speechResumeRig) speak(t *testing.T) {
	t.Helper()
	r.out, r.finish = playThroughDevice(t, r.device)
}

// opensDuring fails the test if the microphone opens within d, and reports
// the device calls it saw.
func opensDuring(t *testing.T, mic *scriptedMic, d time.Duration, what string) []string {
	t.Helper()
	var calls []string
	deadline := time.After(d)
	for {
		select {
		case call := <-mic.events:
			calls = append(calls, call)
			if call == "open" || call == "start" || call == "reopen" {
				t.Fatalf("the microphone opened %s: device calls %v", what, calls)
			}
		case <-deadline:
			return calls
		}
	}
}

// TestTheJabraRingNeverShowsMutedWhileTheMicrophoneIsOpen fails if a resume
// during speech opens the microphone while the Jabra's ring still shows
// Muted. The light cannot change while the speech's playback stream is open,
// and a ring that reads Muted while the unit records tells the people at the
// table they are not recorded.
func TestTheJabraRingNeverShowsMutedWhileTheMicrophoneIsOpen(t *testing.T) {
	r := newSpeechResumeRig(t)
	mic := r.startMicLoop(t)
	r.speak(t)

	r.capture.Resume()
	deadline := time.After(200 * time.Millisecond)
speech:
	for {
		select {
		case call := <-mic.events:
			if call == "open" && r.ring.Load() {
				t.Fatal("the microphone opened while the Jabra's ring showed Muted")
			}
		case <-deadline:
			break speech
		}
	}
	r.finish()
	mic.expectEvents(t, "open")
	if r.ring.Load() {
		t.Fatal("the microphone opened after the speech while the Jabra's ring showed Muted")
	}
}

// TestAResumeDuringSpeechWaitsForPlaybackToClose fails if a resume during
// speech opens the microphone, or confirms the recording light red, before
// the speech's playback stream closes, or reports capture as anything but
// resuming while it waits, also past the stall limit. It covers the mic loop's
// resume and the first resume after a paused start, which starts the
// microphone through startMicrophone.
func TestAResumeDuringSpeechWaitsForPlaybackToClose(t *testing.T) {
	t.Run("mic loop", func(t *testing.T) {
		r := newSpeechResumeRig(t)
		mic := r.startMicLoop(t)
		r.speak(t)

		r.capture.Resume()
		waitOpen(t, r.capture)
		opensDuring(t, mic, 100*time.Millisecond, "while TTS playback was open")
		if got := r.reds.Load(); got != 0 {
			t.Fatalf("the recording light was confirmed red %d times while the resume waited for the speech", got)
		}
		if got := r.capture.captureState(fakeMicState(false)); got != captureResuming {
			t.Fatalf("capture state while the resume waits for the speech, past the stall limit = %q, want %q", got, captureResuming)
		}
		r.finish()
		mic.expectEvents(t, "open")
		if got := r.reds.Load(); got != 1 {
			t.Fatalf("red confirmations once the speech ended = %d, want 1, before the microphone opened", got)
		}
	})
	t.Run("first resume after a paused start", func(t *testing.T) {
		r := newSpeechResumeRig(t)
		started := make(chan bool, 1)
		ctx, cancel := context.WithCancel(context.Background())
		var loops sync.WaitGroup
		t.Cleanup(func() { cancel(); loops.Wait() })
		booted := make(chan struct{})
		close(booted)
		if !startMicrophone(ctx, &loops, r.capture, booted, true, func(bool) error {
			started <- r.ring.Load()
			return nil
		}, func() { t.Error("the start-up retried, but no attempt failed") }) {
			t.Fatal("control: a paused start did not defer the microphone")
		}
		r.speak(t)

		r.capture.Resume()
		waitOpen(t, r.capture)
		select {
		case <-started:
			t.Fatal("the microphone started while TTS playback was open")
		case <-time.After(100 * time.Millisecond):
		}
		r.finish()
		select {
		case muted := <-started:
			if muted {
				t.Fatal("the microphone started after the speech while the Jabra's ring showed Muted")
			}
		case <-time.After(testWait):
			t.Fatal("the microphone did not start once the speech ended")
		}
	})
}

// TestAPauseDuringTheSpeechWaitCancelsTheResume fails if a resume that waits
// for speech to end opens the microphone after a pause came during the wait:
// the pause wins, the microphone stays closed and the ring shows Muted.
func TestAPauseDuringTheSpeechWaitCancelsTheResume(t *testing.T) {
	t.Run("mic loop", func(t *testing.T) {
		r := newSpeechResumeRig(t)
		mic := r.startMicLoop(t)
		r.speak(t)

		r.capture.Resume()
		waitOpen(t, r.capture)
		time.Sleep(30 * time.Millisecond)
		r.capture.Pause()
		r.finish()
		opensDuring(t, mic, 200*time.Millisecond, "after a pause during the wait for the speech")
		if !r.capture.IsPaused() || !r.ring.Load() {
			t.Fatalf("after the pause: paused %t, ring shows Muted %t, want both", r.capture.IsPaused(), r.ring.Load())
		}
	})
	t.Run("first resume after a paused start", func(t *testing.T) {
		r := newSpeechResumeRig(t)
		var starts atomic.Int32
		ctx, cancel := context.WithCancel(context.Background())
		var loops sync.WaitGroup
		t.Cleanup(func() { cancel(); loops.Wait() })
		booted := make(chan struct{})
		close(booted)
		startMicrophone(ctx, &loops, r.capture, booted, true, func(bool) error {
			starts.Add(1)
			return nil
		}, func() { t.Error("the start-up retried, but no attempt failed") })
		r.speak(t)

		r.capture.Resume()
		waitOpen(t, r.capture)
		time.Sleep(30 * time.Millisecond)
		r.capture.Pause()
		r.finish()
		time.Sleep(200 * time.Millisecond)
		if got := starts.Load(); got != 0 {
			t.Fatalf("the microphone started %d times after a pause during the wait for the speech", got)
		}
		// The next resume, with no speech, starts it.
		r.capture.Resume()
		waitForHardware(t, "the next resume to start the microphone", func() bool { return starts.Load() == 1 })
	})
}
