package main

import (
	"slices"
	"testing"
	"time"
)

// TestListeningFrameReachesNoGateWriter fails if audio the paused listener
// captures in listening reaches any writer behind the capture gate: the
// recorder, its preroll, Deepgram's send queue, or the voice's tap, which
// feeds the voices and the Concierge. It runs the real capture pipeline with
// fake microphones. Control: while recording, each writer gets audio.
func TestListeningFrameReachesNoGateWriter(t *testing.T) {
	r := startMicStatesRig(t, statesOptions{listener: true, jabra: true, pipeline: true})
	waitForStates(t, "audio to reach every gate writer while recording", func() bool {
		return r.sent.n.Load() > 0 && r.tapped.n.Load() > 0 && r.recorder.Pending() > 0
	})

	r.moveTo(t, micListening)
	// What passed the gate before it closed may still drain from the send
	// queue; count from once the listener holds the microphone.
	time.Sleep(100 * time.Millisecond)
	sent, tapped, heard := r.sent.n.Load(), r.tapped.n.Load(), r.spotter.accepted.Load()
	time.Sleep(500 * time.Millisecond) // the room talks; nothing it says is the phrase
	if got := r.spotter.accepted.Load(); got <= heard {
		t.Fatalf("control: the listener read no audio in listening (%d samples before, %d after)", heard, got)
	}
	if r.sent.n.Load() != sent || r.tapped.n.Load() != tapped {
		t.Fatalf("audio reached a gate writer while listening: Deepgram %d -> %d bytes, the tap %d -> %d bytes", sent, r.sent.n.Load(), tapped, r.tapped.n.Load())
	}
	if got := r.recorder.Pending(); got != 0 {
		t.Fatalf("the preroll holds %v of audio while listening", got)
	}
	if state := r.capture.micState(); state != micListening {
		t.Fatalf("control: the unit left listening during the check, to %v", state)
	}
	// Control: the phrase resumes, and audio reaches the writers again.
	r.saidPhrase()
	r.requireState(t, micRecording, "the phrase")
	waitForStates(t, "audio to reach the gate writers once recording again", func() bool {
		return r.sent.n.Load() > sent && r.tapped.n.Load() > tapped
	})
}

// TestListenerHandoverOrder fails if the listener's stream and the recording
// microphone are ever open together, in either order: the listener closes
// before the recording microphone prepares to open, opens or PortAudio scans,
// and opens only once the recording microphone has closed, also when that
// close first fails because a read still runs (audio.ErrMicStillReading).
func TestListenerHandoverOrder(t *testing.T) {
	r := startMicStatesRig(t, statesOptions{listener: true, jabra: true})

	// Recording to listening, with a read that does not return: the pause's
	// close cannot release the microphone at first.
	select {
	case <-r.recMic.stick():
	case <-time.After(testWait):
		t.Fatal("control: the recording microphone's read did not block")
	}
	if err := r.hooks.Pause(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if r.listenerOpen.Load() {
		t.Fatal("the listener opened while the recording microphone could not close")
	}
	r.mu.Lock()
	refused := slices.Contains(r.events, "mic close: still reading")
	r.mu.Unlock()
	if !refused {
		t.Fatal("control: the pause's close released the microphone at once")
	}
	r.recMic.unstick()
	r.requireState(t, micListening, "the recording microphone's close, once its read returned")

	// A PortAudio scan while listening closes the listener first.
	scanned := make(chan bool, 1)
	if err := r.device.probe(func() error {
		r.note("scan")
		scanned <- r.listenerOpen.Load()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if <-scanned {
		t.Fatal("PortAudio scanned with the listener's stream open")
	}
	r.requireState(t, micListening, "the scan, after which the listener opens again")

	// Listening to recording, by the page, the power button and the phrase.
	for _, resume := range []func(){
		func() { _ = r.hooks.Resume() },
		func() { <-r.power.press(nil, nil) },
		r.saidPhrase,
	} {
		resume()
		r.requireState(t, micRecording, "a resume from listening")
		if err := r.hooks.Pause(); err != nil {
			t.Fatal(err)
		}
		r.requireState(t, micListening, "a pause from recording")
	}

	r.mu.Lock()
	events := slices.Clone(r.events)
	r.mu.Unlock()
	for i, event := range events {
		if event != "mic open" {
			continue
		}
		for j := i - 1; j >= 0; j-- {
			if events[j] == "listener open" {
				t.Fatalf("stream events %v: the recording microphone opened (event %d) after the listener's open (event %d) with no close between", events, i, j)
			}
			if events[j] == "listener close" {
				break
			}
		}
	}
	r.requireHandoverKept(t)
}

// requireTheListenerAloneWithNoLight fails if a unit with no recording light
// does not listen once it restores a saved listening, or if the phrase opens
// the recording microphone then: the light is off in listening, which the
// listener needs, and the phrase's resume confirms red first, so it is
// refused and the unit goes back to listening.
func requireTheListenerAloneWithNoLight(t *testing.T) {
	t.Helper()
	r := startMicStatesRig(t, statesOptions{listener: true, jabra: true, saved: savedListening, noLight: true})
	r.requireState(t, micListening, "a saved listening with no light, once the listener started")
	r.saidPhrase()
	time.Sleep(200 * time.Millisecond)
	r.requireState(t, micListening, "the phrase with no light: a refused resume returns to listening")
	r.mu.Lock()
	opens := r.micOpens
	r.mu.Unlock()
	if opens != 0 {
		t.Fatalf("the recording microphone opened %d times with no light", opens)
	}
}
