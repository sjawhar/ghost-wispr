package main

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/jabra"
)

// heldOutput is a TTS playback stream on the Jabra that stays open, writing,
// until release closes. open is true from when the stream opens until it
// closes.
type heldOutput struct {
	writing chan struct{}
	release chan struct{}
	once    sync.Once
	open    atomic.Bool
}

func (h *heldOutput) Start() error { return nil }
func (h *heldOutput) Stop() error  { return nil }
func (h *heldOutput) Close() error { h.open.Store(false); return nil }
func (h *heldOutput) Write() error {
	h.once.Do(func() { close(h.writing) })
	<-h.release
	return nil
}

// playThroughDevice starts a real audio.Speaker, wired to device as main
// wires it, playing into a stream that stays open until the returned finish
// runs. finish waits for Play to return.
func playThroughDevice(t *testing.T, device *captureDevice) (out *heldOutput, finish func()) {
	t.Helper()
	speaker := audio.NewSpeaker("Jabra")
	speaker.SetOutputHooks(device.playbackOpening, device.playbackClosing)
	out = &heldOutput{writing: make(chan struct{}), release: make(chan struct{})}
	speaker.SetOpenStreamForTest(func(int, int, int, []int16, string) (audio.OutputStream, error) {
		out.open.Store(true)
		return out, nil
	})
	played := make(chan struct{})
	go func() {
		defer close(played)
		_, _ = speaker.Play(make([]byte, 64000), audio.AudioFormat{SampleRate: 16000, Channels: 1, Encoding: audio.EncodingPCM16})
	}()
	<-out.writing
	var once sync.Once
	finish = func() {
		once.Do(func() { close(out.release) })
		<-played
	}
	t.Cleanup(finish)
	return out, finish
}

// TestNoJabraLightWriteWhileTTSPlaybackIsOpen fails if the Jabra's light is
// written while TTS playback holds an output stream open on the Jabra
// (deploy/pi/files/ghost-wispr.yaml: speaker_device J75). A light change
// while only a playback stream was open made the Jabra send Phone Mute by
// itself at the next microphone close on the office Pi (INFRA-1027 trace,
// section 9.7, restart 1), and a microphone close while recording, such as a
// reconnect, would read that report as a tap and pause the unit. A pause and a
// resume land during speech here. The "idle" case is the control: with no
// playback the same pause and resume write the light.
func TestNoJabraLightWriteWhileTTSPlaybackIsOpen(t *testing.T) {
	for _, playing := range []bool{true, false} {
		name := "idle"
		if playing {
			name = "tts playing"
		}
		t.Run(name, func(t *testing.T) {
			capture := newCaptureGate(nil, nil, nil)
			mic := newScriptedMic()
			device := newCaptureDevice(capture.IsPaused)
			var out *heldOutput
			if playing {
				out, _ = playThroughDevice(t, device)
			}
			var mu sync.Mutex
			var writes, duringPlayback []string
			detach := device.attach(func(on bool) error {
				mu.Lock()
				defer mu.Unlock()
				write := map[bool]string{true: "light on", false: "light off"}[on]
				writes = append(writes, write)
				if out != nil && out.open.Load() {
					duringPlayback = append(duringPlayback, write)
				}
				return nil
			})
			t.Cleanup(detach)
			if err := device.openStream(func() (bool, error) { return true, nil }); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() {
				defer close(done)
				streamMicWithRetry(ctx, capture, orderedMic{micStreamer: mic, device: device}, io.Discard, func(time.Duration) {}, nopLogf, nil)
			}()
			t.Cleanup(func() { cancel(); close(mic.chunks); <-done })
			mic.expectEvents(t, "stream")

			capture.Pause() // a web or power-button pause while TTS speaks
			mic.capture(t, []byte{1, 2})
			mic.expectEvents(t, "close")
			capture.Resume()
			mic.expectEvents(t, "open", "stream")

			mu.Lock()
			defer mu.Unlock()
			if len(duringPlayback) > 0 {
				t.Fatalf("the Jabra light was written %d times while TTS playback held the Jabra's output stream open: %v", len(duringPlayback), duringPlayback)
			}
			if !playing && len(writes) < 2 {
				t.Fatalf("control: with no playback the pause and resume wrote the light %v, want it written", writes)
			}
		})
	}
}

// TestNoJabraLightWriteDuringTTSOnAnyMicrophonePath fails if a microphone
// path other than a pause and a resume writes the Jabra's light while TTS
// playback is open: the mic loop's reconnect after an overflow, the close of a
// device the recording light could not be confirmed red for, and the close
// when the light guard holds capture. Each of those closes comes while the
// switch is on, so a Jabra report it set off would read as a tap and pause the
// unit; a stream the light never changed under sets off none.
func TestNoJabraLightWriteDuringTTSOnAnyMicrophonePath(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(capture *captureGate, mic *scriptedMic)
		events []string
		then   func(t *testing.T, capture *captureGate, mic *scriptedMic)
	}{
		{
			name: "reconnect after an overflow",
			setup: func(_ *captureGate, mic *scriptedMic) {
				mic.streamErrs = []error{errors.New("Input overflowed")}
			},
			// The loop closes the device before it waits to reconnect.
			events: []string{"stream", "close", "reopen", "stream"},
		},
		{
			name: "close when red cannot be confirmed before a reconnect",
			setup: func(capture *captureGate, mic *scriptedMic) {
				capture.beforeMicStart = func() error { return errors.New("recording light stayed off") }
				mic.streamErrs = []error{errors.New("device disconnected")}
			},
			// The device closes before the wait, and again when the light
			// refuses the reconnect; the loop then waits for the light.
			events: []string{"stream", "close", "close"},
		},
		{
			name:   "close when the light guard holds capture",
			events: []string{"stream"},
			then: func(t *testing.T, capture *captureGate, mic *scriptedMic) {
				capture.HoldForLight()
				mic.capture(t, []byte{1, 2})
				mic.expectEvents(t, "close")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capture := newCaptureGate(nil, nil, nil)
			mic := newScriptedMic()
			if tc.setup != nil {
				tc.setup(capture, mic)
			}
			device := newCaptureDevice(capture.IsPaused)
			out, _ := playThroughDevice(t, device)
			var mu sync.Mutex
			var duringPlayback []string
			detach := device.attach(func(on bool) error {
				mu.Lock()
				defer mu.Unlock()
				if out.open.Load() {
					duringPlayback = append(duringPlayback, map[bool]string{true: "light on", false: "light off"}[on])
				}
				return nil
			})
			t.Cleanup(detach)
			if err := device.openStream(func() (bool, error) { return true, nil }); err != nil {
				t.Fatal(err)
			}
			runMicLoopOn(t, capture, orderedMic{micStreamer: mic, device: device}, mic, io.Discard, (&recordedWaits{}).wait)
			mic.expectEvents(t, tc.events...)
			if tc.then != nil {
				tc.then(t, capture, mic)
			}

			mu.Lock()
			defer mu.Unlock()
			if len(duringPlayback) > 0 {
				t.Fatalf("the Jabra light was written %d times while TTS playback was open: %v", len(duringPlayback), duringPlayback)
			}
		})
	}
}

// runMicLoopOn runs streamMicWithRetry on streamer, whose device calls mic
// reports, into recording, until the test ends.
func runMicLoopOn(t *testing.T, capture *captureGate, streamer micStreamer, mic *scriptedMic, recording io.Writer, wait func(time.Duration)) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		streamMicWithRetry(ctx, capture, streamer, recording, wait, nopLogf, nil)
	}()
	t.Cleanup(func() { cancel(); close(mic.chunks); <-done })
}

// TestJabraLightWaitsForTTSPlaybackToClose fails if a pause during TTS
// playback, with the microphone closed, does not light the Jabra once the
// playback stream closes: the write that the open stream held back must come
// then, not at the next pause or resume.
func TestJabraLightWaitsForTTSPlaybackToClose(t *testing.T) {
	rig := newJabraRig(false)
	rig.checkEvery = time.Hour // no light check repairs it
	rig.start(t)
	device := rig.plugIn()
	waitForHardware(t, "the Jabra to connect", func() bool { return device.reportsSent() > 0 })
	out, finish := playThroughDevice(t, rig.device)

	reports := device.reportsSent()
	if err := rig.controls.Pause(); err != nil {
		t.Fatal(err)
	}
	rig.device.refreshLight(jabra.MonotonicNow()) // as a press does
	time.Sleep(20 * time.Millisecond)
	if device.reportsSent() != reports || device.shows() {
		t.Fatalf("the light changed while TTS playback was open: %d light reports, shows %t", device.reportsSent()-reports, device.shows())
	}
	finish()
	if out.open.Load() {
		t.Fatal("control: the playback stream is still open after Play returned")
	}
	waitForHardware(t, "the Jabra light to show the pause once playback closed", device.shows)
}
