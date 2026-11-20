package main

import (
	"context"
	"io"
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
	r.device = newCaptureDevice(r.capture.inClosedState)
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
	runMicLoopOn(t, r.capture, orderedMic{micStreamer: mic, device: r.device}, mic, io.Discard, (&recordedWaits{}).wait)
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
		deferred, _ := startMicrophone(ctx, &loops, r.capture, booted, func(bool) error {
			started <- r.ring.Load()
			return nil
		}, func() { t.Error("the start-up retried, but no attempt failed") })
		if !deferred {
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
// the pause wins and the microphone stays closed. The ring then shows
// ringMuted: Muted for a pause to closed, and not for a pause to listening,
// where the paused listener holds the microphone.
func TestAPauseDuringTheSpeechWaitCancelsTheResume(t *testing.T) {
	for _, tc := range []struct {
		name      string
		pause     func(*captureGate)
		state     micState
		ringMuted bool
	}{
		{"mic loop", (*captureGate).Pause, micClosed, true},
		{"mic loop, a pause to listening", (*captureGate).PauseListening, micListening, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newSpeechResumeRig(t)
			mic := r.startMicLoop(t)
			r.speak(t)

			r.capture.Resume()
			waitOpen(t, r.capture)
			time.Sleep(30 * time.Millisecond)
			tc.pause(r.capture)
			r.finish()
			opensDuring(t, mic, 200*time.Millisecond, "after a pause during the wait for the speech")
			if got := r.capture.micState(); got != tc.state || r.ring.Load() != tc.ringMuted {
				t.Fatalf("after the pause: %v, ring shows Muted %t; want %v, %t", got, r.ring.Load(), tc.state, tc.ringMuted)
			}
		})
	}
	t.Run("first resume after a paused start", func(t *testing.T) {
		r := newSpeechResumeRig(t)
		var starts atomic.Int32
		ctx, cancel := context.WithCancel(context.Background())
		var loops sync.WaitGroup
		t.Cleanup(func() { cancel(); loops.Wait() })
		booted := make(chan struct{})
		close(booted)
		_, _ = startMicrophone(ctx, &loops, r.capture, booted, func(bool) error {
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

// TestAResumeLightsTheJabraWhenSpeechStartsDuringItsWrite fails if a speech
// that starts just as the resume lights the Jabra makes the resume open the
// microphone with the ring still showing Muted. A speech's playbackOpening
// holds the light lock through its settle, then counts the playback; if the
// resume has already found no playback, its light write finds one and writes
// nothing. The test holds the light lock and counts the playback as
// playbackOpening does, so the order is the same on every run. The control
// lets the resume write the light first.
func TestAResumeLightsTheJabraWhenSpeechStartsDuringItsWrite(t *testing.T) {
	for _, speechFirst := range []bool{false, true} {
		name := "control, the resume writes the light first"
		if speechFirst {
			name = "a speech takes the light lock first"
		}
		t.Run(name, func(t *testing.T) {
			r := newSpeechResumeRig(t)
			mic := r.startMicLoop(t)
			time.Sleep(2 * jabraLightSettle) // the pause's light write has settled

			if speechFirst {
				r.device.lightMu.Lock()
			}
			r.capture.Resume()
			waitOpen(t, r.capture)
			if speechFirst {
				time.Sleep(50 * time.Millisecond) // the resume found no playback and waits for the light lock
				r.device.mu.Lock()
				r.device.playing++ // as playbackOpening does under the light lock
				r.device.mu.Unlock()
				r.device.lightMu.Unlock()
				time.Sleep(100 * time.Millisecond) // the speech plays
				r.device.playbackClosing(func() error { return nil })
			}
			mic.expectEvents(t, "open")
			if r.ring.Load() {
				t.Fatal("the microphone opened while the Jabra's ring showed Muted")
			}
		})
	}
}

// TestACancelledSpeechWaitThenAQuickResumeStillWaits fails if the mic loop,
// whose wait for speech a pause cancelled, uses the microphone it closed
// without waiting again, because a second resume opened the gate before the
// loop looked: it would stream the closed microphone, and the reconnect after
// that error would open it during the speech with the ring showing Muted. The
// test holds the loop for that moment after the cancel.
func TestACancelledSpeechWaitThenAQuickResumeStillWaits(t *testing.T) {
	r := newSpeechResumeRig(t)
	cancelled := make(chan struct{})
	proceed := make(chan struct{})
	var held atomic.Bool
	prepare := r.capture.beforeMicOpen
	r.capture.beforeMicOpen = func(ctx context.Context, closing <-chan struct{}) bool {
		if prepare(ctx, closing) {
			return true
		}
		if held.CompareAndSwap(false, true) {
			close(cancelled)
			<-proceed
		}
		return false
	}
	var released atomic.Bool
	release := func() {
		if released.CompareAndSwap(false, true) {
			close(proceed)
		}
	}
	t.Cleanup(release)
	mic := r.startMicLoop(t)
	r.speak(t)

	r.capture.Resume()
	waitOpen(t, r.capture)
	time.Sleep(20 * time.Millisecond) // the resume waits for the speech
	r.capture.Pause()
	select {
	case <-cancelled:
	case <-time.After(testWait):
		t.Fatal("control: the pause did not cancel the wait for the speech")
	}
	r.capture.Resume() // before the mic loop looks again
	waitOpen(t, r.capture)
	release()

	var calls []string
	deadline := time.After(150 * time.Millisecond) // the speech plays on
speech:
	for {
		select {
		case call := <-mic.events:
			calls = append(calls, call)
			if call != "close" {
				t.Fatalf("the mic loop used the microphone during the speech, after a cancelled wait: device calls %v", calls)
			}
		case <-deadline:
			break speech
		}
	}
	r.finish()
	for {
		select {
		case call := <-mic.events:
			calls = append(calls, call)
			switch call {
			case "close":
				continue
			case "open":
				if r.ring.Load() {
					t.Fatal("the microphone opened while the Jabra's ring showed Muted")
				}
				return
			default:
				t.Fatalf("the microphone was used before it opened again: device calls %v", calls)
			}
		case <-time.After(testWait):
			t.Fatalf("the microphone did not open once the speech ended: device calls %v", calls)
		}
	}
}

// ringCheckingRecording stands for the recording and Deepgram: it counts the
// bytes that reach it, and those that reach it while the Jabra's ring shows
// Muted.
type ringCheckingRecording struct {
	ring         *atomic.Bool
	bytes, muted atomic.Int64
}

func (w *ringCheckingRecording) Write(p []byte) (int, error) {
	if w.ring.Load() {
		w.muted.Add(int64(len(p)))
	}
	w.bytes.Add(int64(len(p)))
	return len(p), nil
}

// TestNoAudioPassesWhileTheJabraRingShowsMuted fails if audio reaches the
// recording while the Jabra's ring shows Muted, when a pause and a resume land
// while the mic loop opens the microphone: the pause lights the Jabra just
// before the open, the resume opens the gate during the open's light settle,
// and the light cannot change once the microphone is open. The microphone must
// be closed, the Jabra darkened and the microphone opened again before audio
// passes. The pause comes after the open's mark (markMicOpen), so the loop
// closes that microphone after its open, before it streams any buffer: the
// test fails if the loop streams it instead (the check after the open), not
// if the ring guard (ringGuard) lets audio pass, which this race no longer
// reaches. TestAJabraThatConnectsWhileRecordingIsLitBeforeAudioPasses tests
// the ring guard. The control has no pause during the open.
func TestNoAudioPassesWhileTheJabraRingShowsMuted(t *testing.T) {
	for _, race := range []bool{false, true} {
		name := "control, no pause during the open"
		if race {
			name = "a pause and a resume during the open"
		}
		t.Run(name, func(t *testing.T) {
			capture := newCaptureGate(nil, nil, nil)
			capture.Pause()
			device := newCaptureDevice(capture.IsPaused)
			capture.beforeMicOpen = device.prepareLightForOpen
			var ring, armed, paused atomic.Bool
			detach := device.attach(func(on bool) error {
				ring.Store(on)
				if on && armed.CompareAndSwap(true, false) {
					go capture.Resume() // during the open's light settle
				}
				return nil
			})
			t.Cleanup(detach)
			capture.beforeMicStart = func() error {
				if race && paused.CompareAndSwap(false, true) {
					armed.Store(true)
					capture.Pause() // after the resume darkened the Jabra, before the open
				}
				return nil
			}
			mic := newScriptedMic()
			// The ring as each open began: the loop closes the open a pause
			// came during, and darkens the ring, before the test can look.
			ringAtOpen := make(chan bool, 4)
			opening := ringAtOpenMic{scriptedMic: mic, ring: &ring, atOpen: ringAtOpen}
			recording := &ringCheckingRecording{ring: &ring}
			runMicLoopOn(t, capture, orderedMic{micStreamer: opening, device: device}, mic, recording, (&recordedWaits{}).wait)
			mic.expectEvents(t, "close")

			capture.Resume()
			mic.expectEvents(t, "open")
			if muted := <-ringAtOpen; race && !muted {
				t.Fatal("control: the microphone opened with the ring dark; the pause did not land before the open")
			}
			if race {
				mic.expectEvents(t, "close", "open")
				if <-ringAtOpen {
					t.Fatal("the microphone opened again with the Jabra's ring showing Muted")
				}
			}
			mic.expectEvents(t, "stream")
			mic.capture(t, make([]byte, 320))
			if got := recording.muted.Load(); got > 0 {
				t.Fatalf("%d bytes reached the recording while the Jabra's ring showed Muted", got)
			}
			if got := recording.bytes.Load(); got != 320 {
				t.Fatalf("the recording got %d bytes once the ring was dark, want 320", got)
			}
		})
	}
}

// ringAtOpenMic is a capture device that sends, as each Open begins, whether
// the Jabra's ring shows Muted.
type ringAtOpenMic struct {
	*scriptedMic
	ring   *atomic.Bool
	atOpen chan<- bool
}

func (m ringAtOpenMic) Open() error {
	m.atOpen <- m.ring.Load()
	return m.scriptedMic.Open()
}

// TestAJabraThatConnectsWhileRecordingIsLitBeforeAudioPasses fails if audio
// reaches the recording while a Jabra that connected with the microphone open
// may show Muted: its light cannot be written until the microphone closes, and
// its ring keeps whatever it showed before it connected.
func TestAJabraThatConnectsWhileRecordingIsLitBeforeAudioPasses(t *testing.T) {
	capture := newCaptureGate(nil, nil, nil)
	device := newCaptureDevice(capture.IsPaused)
	capture.beforeMicOpen = device.prepareLightForOpen
	var ring atomic.Bool
	if err := device.openStream(func() (bool, error) { return true, nil }); err != nil {
		t.Fatal(err) // the microphone opened at start-up, before the Jabra connected
	}
	mic := newScriptedMic()
	recording := &ringCheckingRecording{ring: &ring}
	runMicLoopOn(t, capture, orderedMic{micStreamer: mic, device: device}, mic, recording, (&recordedWaits{}).wait)
	mic.expectEvents(t, "stream")
	mic.capture(t, make([]byte, 320))
	if got := recording.bytes.Load(); got != 320 {
		t.Fatalf("control: with no Jabra the recording got %d bytes, want 320", got)
	}

	ring.Store(true) // the Jabra shows Muted from before it connected
	detach := device.attach(func(on bool) error { ring.Store(on); return nil })
	t.Cleanup(detach)
	if !ring.Load() {
		t.Fatal("control: the Jabra's light was written while the microphone was open")
	}
	mic.capture(t, make([]byte, 320))
	if got := recording.muted.Load(); got > 0 {
		t.Fatalf("%d bytes reached the recording while the Jabra's ring showed Muted", got)
	}
	mic.expectEvents(t, "close", "open", "stream")
	if ring.Load() {
		t.Fatal("the microphone opened again with the Jabra's ring showing Muted")
	}
	mic.capture(t, make([]byte, 320))
	if got := recording.bytes.Load(); got != 640 {
		t.Fatalf("the recording got %d bytes, want 640: 320 before the Jabra connected and 320 once its ring was dark", got)
	}
}

// TestTheRingWaitForSpeechReportsResuming fails if the mic loop, which closed
// the microphone because a Jabra connected during speech while recording,
// reports the microphone as unavailable while it waits for the speech to end
// before it darkens the ring and opens the microphone again. Nothing records
// meanwhile, and the blink(1) is off; readiness treats mic_unavailable as a
// fault, and the wait is not one.
func TestTheRingWaitForSpeechReportsResuming(t *testing.T) {
	capture := newCaptureGate(nil, nil, nil)
	device := newCaptureDevice(capture.IsPaused)
	capture.beforeMicOpen = device.prepareLightForOpen
	var ring atomic.Bool
	if err := device.openStream(func() (bool, error) { return true, nil }); err != nil {
		t.Fatal(err)
	}
	mic := newScriptedMic()
	recording := &ringCheckingRecording{ring: &ring}
	runMicLoopOn(t, capture, orderedMic{micStreamer: mic, device: device}, mic, recording, (&recordedWaits{}).wait)
	mic.expectEvents(t, "stream")
	_, finish := playThroughDevice(t, device) // the unit speaks on the Jabra

	ring.Store(true) // the Jabra connects showing Muted
	detach := device.attach(func(on bool) error { ring.Store(on); return nil })
	t.Cleanup(detach)
	mic.capture(t, make([]byte, 320))
	mic.expectEvents(t, "close")
	opensDuring(t, mic, 50*time.Millisecond, "while TTS playback was open")
	if got := capture.captureState(fakeMicState(false)); got != captureResuming {
		t.Fatalf("capture state while the microphone waits for the speech to end = %q, want %q", got, captureResuming)
	}
	finish()
	mic.expectEvents(t, "open", "stream")
	if ring.Load() {
		t.Fatal("the microphone opened again with the Jabra's ring showing Muted")
	}
	if got := recording.muted.Load(); got > 0 {
		t.Fatalf("%d bytes reached the recording while the Jabra's ring showed Muted", got)
	}
}
