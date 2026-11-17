package main

import (
	"io"
	"testing"
	"time"
)

// TestAPauseReleasesTheCaptureDevice fails if a pause leaves the capture
// device open. docs/pi-setup.md says "Ghost Wispr closes the microphone while
// paused", and POST /api/pause says the microphone stops. But
// holdMicWhilePaused only calls Stop (Pa_StopStream), which keeps the PortAudio
// stream, and with it the ALSA capture device, open. On the office Pi
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
			if call == "start" || call == "reopen" || call == "stream" {
				t.Fatalf("the device was used again while paused before it was released: %v", calls)
			}
			continue
		case <-deadline:
		}
		break
	}
	t.Fatalf("a pause left the capture device open: device calls while paused %v, want a close", calls)
}
