package voice

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

// Sol's review of #195 at 48ca91a1 left these probes. The never mind probe
// failed there; the capture boundary and the gate close passed, and stay as
// regression tests.

// TestReview195UncanceledQuestionControl is the control of the never mind
// probe below: a question answered to its end keeps its words.
func TestReview195UncanceledQuestionControl(t *testing.T) {
	c := newFakeConcierge("A normal answer.")
	c.outcome = OutcomeAnswered
	r, log := newQuestionRig(t, c, false)
	sess := r.ask("what about the cancelled-words-marker?")
	waitSaid(t, sess)
	r.speaker.finish(audio.InputVoice)
	if q := log.next(t); q.Heard != "what about the cancelled-words-marker?" || q.Outcome != OutcomeAnswered {
		t.Fatalf("control: logged %+v", q)
	}
}

// TestReview195NeverMindDoesNotKeepCanceledWords fails if a question
// "Wispr, never mind" cancels is logged with any of its words: words the
// room takes back are used and sent nowhere, and never stored (#191's rule).
// It is logged, as never_mind, with its times alone.
func TestReview195NeverMindDoesNotKeepCanceledWords(t *testing.T) {
	c := newFakeConcierge()
	c.hold = true
	r, log := newQuestionRig(t, c, false)
	r.ask("what about the cancelled-words-marker?")
	r.heard("Wispr, never mind.", true, true)
	q := log.next(t)
	if q.Outcome != OutcomeNeverMind {
		t.Fatalf("logged %+v, want never_mind", q)
	}
	raw, err := json.Marshal(q)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "cancelled-words-marker") {
		t.Fatalf("the cancelled words were logged: %s", raw)
	}
}

// TestReview195QuestionLogCaptureBoundary fails if a call heard while the
// capture gate is closed, the light is off, or from before the gate's latest
// closing opens a voice or logs a question.
func TestReview195QuestionLogCaptureBoundary(t *testing.T) {
	for _, name := range []string{"paused", "light off", "stale before pause"} {
		t.Run(name, func(t *testing.T) {
			r, log := newQuestionRig(t, newFakeConcierge("No."), false)
			tr := transcriptOf("Wispr, what about the boundary-words-marker?", 0, true, true, 0)
			switch name {
			case "paused":
				tr.GateOpen = false
			case "light off":
				r.setRed(false)
			case "stale before pause":
				r.closeGate()
			}
			r.s.OnTranscript(&tr)
			log.none(t, name, 100*time.Millisecond)
			if len(r.voice.opened) != 0 {
				t.Fatalf("a voice opened while %s", name)
			}
		})
	}
}

// TestReview195ClosingGateLogsOneStoppedQuestion fails if a gate closing,
// a pause or a light hold, over an answer still being made logs the
// question other than once, as stopped.
func TestReview195ClosingGateLogsOneStoppedQuestion(t *testing.T) {
	c := newFakeConcierge("A pending answer.")
	c.hold, c.outcome = true, OutcomeAnswered
	r, log := newQuestionRig(t, c, false)
	sess := r.ask("what about the pending-words-marker?")
	waitSaid(t, sess)
	r.closeGate()
	if q := log.next(t); q.Outcome != OutcomeStopped {
		t.Fatalf("logged %+v at the gate's closing, want stopped", q)
	}
	log.none(t, "a second row for the gate's closing", 100*time.Millisecond)
}
