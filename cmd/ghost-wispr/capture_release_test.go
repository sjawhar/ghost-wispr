package main

import (
	"context"
	"io"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// TestAPauseReleasesTheCaptureDevice fails if a pause leaves the capture
// device open. docs/pi-setup.md says Ghost Wispr closes the microphone while
// paused, and POST /api/pause says the microphone closes. a889869's
// holdMicWhilePaused called only Stop (Pa_StopStream), which keeps the
// PortAudio stream, and with it the ALSA capture device, open. On the office Pi
// (a889869, 2026-11-17) after a resume and a pause through the API:
//
//	06:03:37.493Z pause HTTP 204
//	06:03:40.514Z paused: {"capture_state":"paused","paused":true} capture=[pcmC3D1c] light=off
//	              /proc/asound/card3/pcm1c/sub0/status: state: RUNNING
//	06:03:52.584Z paused: … capture=[pcmC3D1c] … state: RUNNING
//
// and on the Jabra itself at 05:51-05:55Z: /dev/snd/pcmC0D0c open, state
// RUNNING, while paused. A unit that restarts paused opens none
// (pcm0c: closed). While the device stays open the Jabra treats its
// microphone as in use: its own Phone Mute report when that stream finally
// closes is what resumed a paused unit
// (TestAPausedUnitStaysPausedWhenTheJabraReportsMuteOnItsOwn).
func TestAPauseReleasesTheCaptureDevice(t *testing.T) {
	capture := newCaptureGate(nil, nil, nil)
	mic := newScriptedMic()
	runMicLoop(t, capture, mic, io.Discard, (&recordedWaits{}).wait, nil)
	mic.expectEvents(t, "stream")

	capture.Pause()
	mic.capture(t, []byte{1, 2, 3, 4})
	var calls []string
	deadline := time.After(300 * time.Millisecond)
	for {
		select {
		case call := <-mic.events:
			calls = append(calls, call)
			if call == "close" {
				return
			}
			if call == "start" || call == "open" || call == "reopen" || call == "stream" {
				t.Fatalf("the device was used again while paused before it was released: %v", calls)
			}
			continue
		case <-deadline:
		}
		break
	}
	t.Fatalf("a pause left the capture device open: device calls while paused %v, want a close", calls)
}

// TestAPauseSendsNoAudioFrameToDeepgramOrTheRecording fails if, after a pause,
// an audio frame reaches the Deepgram socket or a byte reaches the session's
// recording. The chain is the production one from the mic loop on: the
// capture gate, the recorder's writer, the bounded send queue and the
// app-owned Deepgram socket, against a fake Deepgram that counts the binary
// frames, which carry audio. The socket itself may stay connected: its
// KeepAlive and Finalize are text frames.
func TestAPauseSendsNoAudioFrameToDeepgramOrTheRecording(t *testing.T) {
	fake, host := startDeepgramEndpoint(t)
	socket := dialDeepgramEndpoint(t, host, nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sender := transcribe.NewAsyncSender(ctx, socket, deepgramSendChunks, nopLogf)
	dir := t.TempDir()
	recorder := audio.NewRecorder(dir)
	if err := recorder.StartSession("meeting"); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	t.Cleanup(func() { _, _ = recorder.EndSession("meeting") })
	recording := filepath.Join(dir, "meeting.pcm")
	sessions := &fakeSessions{log: newCallLog(), active: "meeting"}
	capture := newSessionCaptureGate(ctx, sessions, fakePreroll{sessions.log}, func() deepgramFinalizer { return sender }, 50*time.Millisecond, testEndRetry, nopLogf)
	mic := newScriptedMic()
	runMicLoop(t, capture, mic, recorder.Writer(sender), (&recordedWaits{}).wait, nil)
	mic.expectEvents(t, "stream")

	// Control: a buffer before the pause reaches Deepgram and the recording.
	mic.capture(t, make([]byte, 3200))
	for deadline := time.Now().Add(testWait); fake.audioFrames() != 1; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("control: Deepgram got %d audio frames before the pause, want 1", fake.audioFrames())
		}
	}
	if got := fileSize(t, recording); got != 3200 {
		t.Fatalf("control: the recording has %d bytes before the pause, want 3200", got)
	}

	capture.Pause()
	mic.capture(t, make([]byte, 3200)) // the device's next buffer
	mic.expectEvents(t, "close")
	sessions.log.waitFor(t, "end-session")
	// A Finalize queued now follows every buffer queued before it, so once
	// Deepgram has it, any audio the pause let through has arrived too.
	if err := sender.Finalize(); err != nil {
		t.Fatalf("Finalize after the pause: %v", err)
	}
	finalizes := func() int {
		return len(slices.DeleteFunc(fake.received(), func(frame string) bool { return frame != "Finalize" }))
	}
	for deadline := time.Now().Add(testWait); finalizes() < 2; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("Deepgram got %q, want the pause's Finalize and the test's", fake.received())
		}
	}
	if got := fake.audioFrames(); got != 1 {
		t.Fatalf("Deepgram got %d audio frames, %d of them after the pause, want none after it", got, got-1)
	}
	if got := fileSize(t, recording); got != 3200 {
		t.Fatalf("the recording has %d bytes after the pause, want the 3200 from before it", got)
	}
}
