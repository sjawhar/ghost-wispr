package main

import (
	"slices"
	"testing"
	"time"
)

// The three states of the microphone and the waiting light (LED 2 of the
// blink(1), which flashes green while an agent's answer waits): LED 2 flashes
// only while the unit records, so never in listening or in closed, where both
// LEDs are off; and a session that ends because the unit left recording drops
// its held answers, as a pause did before the listener.

var lightOff, lightRed = [3]uint8{}, [3]uint8{255, 0, 0}

// TestTheWaitingLightFollowsTheThreeStates fails if, while an answer waits
// (the voice's waiting light on, as main wires it), LED 2 is written green
// while LED 1 is not red anywhere on this tour: recording, listening, back to
// recording by "Wispr, start recording", closed, and back by the Jabra's mute
// button. It also fails if, in listening or in closed, LED 2 is written at all
// or either LED is not off, or if the flash does not come back once the unit
// records again. The flash in recording at the start is the control that the
// rig sees a flash.
func TestTheWaitingLightFollowsTheThreeStates(t *testing.T) {
	r := startMicStatesRig(t, statesOptions{listener: true, jabra: true})
	waitingLED{r.hardware}.On()
	r.requireFlash(t, "recording, at the start")
	for _, step := range []struct {
		state micState
		back  string
		do    func(t *testing.T, r *micStatesRig)
	}{
		{micListening, "Wispr, start recording", func(_ *testing.T, r *micStatesRig) { r.saidPhrase() }},
		{micClosed, "the Jabra's mute button", func(t *testing.T, r *micStatesRig) { r.pressJabra(t) }},
	} {
		r.moveTo(t, step.state)
		r.requireDark(t, step.state)
		step.do(t, r)
		r.requireState(t, micRecording, step.back+" from "+step.state.String())
		r.requireFlash(t, "recording after "+step.back+" from "+step.state.String())
	}
	r.light.mu.Lock()
	defer r.light.mu.Unlock()
	for i, colour := range r.light.led2Writes {
		if colour == led2Green && r.light.led1AtLED2[i] != lightRed {
			t.Fatalf("LED 2 written green with LED 1 %v (write %d of %v)", r.light.led1AtLED2[i], i, r.light.led2Writes)
		}
	}
}

// requireFlash waits for a green write of LED 2 after the call.
func (r *micStatesRig) requireFlash(t *testing.T, where string) {
	t.Helper()
	_, before := r.light.secondLED()
	waitForStates(t, "LED 2 to flash green in "+where, func() bool {
		_, writes := r.light.secondLED()
		return slices.Contains(writes[len(before):], led2Green)
	})
}

// requireDark fails unless, for longer than two of the flash's 500 ms steps
// in state, nothing writes LED 2 and both LEDs stay off.
func (r *micStatesRig) requireDark(t *testing.T, state micState) {
	t.Helper()
	_, before := r.light.secondLED()
	for range 3 {
		r.hardware.Wake()
		time.Sleep(400 * time.Millisecond)
	}
	r.light.mu.Lock()
	defer r.light.mu.Unlock()
	if after := r.light.led2Writes[len(before):]; len(after) != 0 || r.light.colour != lightOff || r.light.led2 != lightOff {
		t.Fatalf("in %v with an answer waiting: LED 2 written %v, LED 1 %v, LED 2 %v; want no write and both off", state, after, r.light.colour, r.light.led2)
	}
}

// TestLeavingRecordingEndsTheSessionForTheVoice fails if moving the unit from
// recording to listening, or to closed, does not end the recording session
// and tell the voice and the Concierge, wired as main wires them
// (wireVoiceSessionHooks). The voice then drops the session's held answers and
// turns its waiting light off (voice.Supervisor.SessionEnded), as after a pause
// before the listener.
func TestLeavingRecordingEndsTheSessionForTheVoice(t *testing.T) {
	for _, state := range []micState{micListening, micClosed} {
		t.Run(state.String(), func(t *testing.T) {
			r := startMicStatesRig(t, statesOptions{listener: true, jabra: true, sessions: true})
			var link voiceLink
			v := &forgetting{}
			link.set(v)
			var hooks conciergeHooks
			c := &recordedSessions{}
			hooks.set(c)
			wireVoiceSessionHooks(r.sessions, &hooks, &link)
			id, err := r.sessions.ManualStartSession(t.Context(), "")
			if err != nil {
				t.Fatal(err)
			}

			r.moveTo(t, state)
			waitForStates(t, "the voice and the Concierge told that session "+id+" ended", func() bool {
				v.mu.Lock()
				defer v.mu.Unlock()
				c.mu.Lock()
				defer c.mu.Unlock()
				return slices.Equal(v.ended, []string{id}) && slices.Equal(c.ended, []string{id})
			})
			if active, _ := r.sessions.ActiveSession(); active != "" {
				t.Fatalf("session %s still active in %v", active, state)
			}
		})
	}
}
