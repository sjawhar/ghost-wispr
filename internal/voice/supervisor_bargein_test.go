package voice

import (
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

// The question of the office runs of 2026-11-21, as Deepgram's live
// transcript wrote it: its final came only after the voice had begun to
// answer.
const lateQuestion = "What's the status of INFRA ten twenty seven?"

// timedResult is a Deepgram result of text whose words are 0.2 s apart from
// start on the socket, the i-th captured at at(i) on the system clock, as
// main.go times a live result's words from the live stream.
func timedResult(text string, start float64, final bool, at func(i int) time.Time) *Transcript {
	tr := transcriptOf(text, start, final, final, 0)
	for i := range tr.Words {
		tr.Words[i].At = at(i)
	}
	return &tr
}

// from times words 0.2 s apart from t0, as people speak them.
func from(t0 time.Time) func(int) time.Time {
	return func(i int) time.Time { return t0.Add(time.Duration(i) * 200 * time.Millisecond) }
}

// speechStart is when the Supervisor heard the voice input's speech start.
func speechStart(r *rig) time.Time {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	return r.s.voiceStarted
}

// waitSpeechAfter waits until the voice input's speech has started again
// after prev, and returns when.
func waitSpeechAfter(t *testing.T, r *rig, prev time.Time) time.Time {
	t.Helper()
	eventually(t, "the voice's next speech", func() bool {
		r.s.mu.Lock()
		defer r.s.mu.Unlock()
		return r.s.voiceSpeaking && r.s.voiceStarted.After(prev)
	})
	return speechStart(r)
}

// voiceCutAfter reports whether the voice input was cut after since, as a
// barge-in cuts it. An exchange that takes the floor cuts the voice input
// too, before its voice speaks (takeFloorLocked): since must come after that.
func voiceCutAfter(r *rig, since time.Time) bool {
	at, ok := r.speaker.cancelledAt(audio.InputVoice)
	return ok && at.After(since)
}

// newLongFollowUpDrivingRig is a driving voice's rig whose follow-up window
// outlasts the test, so the gap between two of the voice's speeches cannot
// end the exchange.
func newLongFollowUpDrivingRig(t *testing.T) *rig {
	t.Helper()
	return newRigWith(t, newFakeConcierge(), func(cfg *SupervisorConfig, r *rig) {
		v := drivingVoice{r.voice}
		cfg.Voice = func() (Voice, error) { return v, nil }
		cfg.IdleWait = time.Minute
		cfg.FollowUpWait = time.Minute
	})
}

// TestTheQuestionsOwnWordsDoNotStopADrivingVoice replays the office runs of
// 2026-11-21 in which the voice cut itself off: "Wispr." alone, then the
// question, whose results Deepgram sent late, while the voice already spoke
// its routing line and then the answer it led. Every word of those results
// was said before the voice began; none is a person talking over it. It
// fails if any of them stops the voice.
func TestTheQuestionsOwnWordsDoNotStopADrivingVoice(t *testing.T) {
	for _, tc := range []struct {
		name string
		wake string
		// The question's results that came during the routing line, then
		// during the answer: final or interim.
		first, second []bool
	}{
		// GPT-Live (session 20261121011203): it spoke before Deepgram
		// finalised the question; an interim came in the routing line, the
		// final 0.36 s into the answer.
		{"GPT-Live", "Wispr.", []bool{false}, []bool{true}},
		// Deepgram's Voice Agent (session 20261121011434): an interim 0.44 s
		// into the routing line ("readi-"), another 0.3 s into the answer,
		// and the final only after both.
		{"Deepgram's Voice Agent", "Whisper.", []bool{false}, []bool{false, true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newLongFollowUpDrivingRig(t)
			woke := time.Now().Add(-4 * time.Second)
			r.s.OnTranscript(timedResult(tc.wake, 192.93, true, from(woke)))
			sess := r.waitSession()
			// The question, 1.6 s after the wake; its last word ends about
			// 0.6 s before the voice speaks.
			asked := woke.Add(1600 * time.Millisecond)
			var first time.Time // the routing line's start
			heard := func(final bool) {
				t.Helper()
				r.s.OnTranscript(timedResult(lateQuestion, 195.11, final, from(asked)))
				never(t, "the question's own words stopped the voice", 300*time.Millisecond, func() bool { return voiceCutAfter(r, first) })
			}

			speakAs(t, sess, "Reading INFRA-1027.")
			first = waitSpeechAfter(t, r, time.Time{})
			for _, final := range tc.first {
				heard(final)
			}
			r.speaker.finish(audio.InputVoice) // the routing line plays out
			speakAs(t, sess, "INFRA-1027 is in review.")
			waitSpeechAfter(t, r, first)
			for _, final := range tc.second {
				heard(final)
			}
			if i, _, _ := sess.state(); !i.IsZero() {
				t.Fatal("the question's own words interrupted the vendor")
			}
		})
	}
}

// TestTheQuestionsLateFinalIsNoFollowUp fails if the question's final, which
// Deepgram sent after a driving voice's answer played out, is taken as the
// room's next turn: it closed the follow-up window, so the exchange stayed
// open for IdleWait and the room's side talk reached the vendor (R10). Its
// words were said before the voice spoke, also when they hold a call of
// their own ("Wispr." then "Wispr, what's …"). The control, words said after
// the answer, keeps the exchange open for the vendor.
func TestTheQuestionsLateFinalIsNoFollowUp(t *testing.T) {
	for _, tc := range []struct {
		name     string
		text     string
		start    float64
		after    bool // the result's words were said after the answer
		stayOpen bool
	}{
		{"the question's late final", lateQuestion, 195.11, false, false},
		{"the late final of a second call", "Wispr, " + lateQuestion, 194.5, false, false},
		{"control: a follow-up said after the answer", lateQuestion, 195.11, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newDrivingRig(t, time.Minute) // FollowUpWait 200 ms
			woke := time.Now().Add(-4 * time.Second)
			r.s.OnTranscript(timedResult("Wispr.", 192.93, true, from(woke)))
			sess := r.waitSession()
			asked := woke.Add(1600 * time.Millisecond)
			r.s.OnTranscript(timedResult(lateQuestion, 195.11, false, from(asked)))
			speakAs(t, sess, "INFRA-1027 is in review.")
			waitSpeechAfter(t, r, time.Time{})
			r.speaker.finish(audio.InputVoice) // the answer plays out: the follow-up window opens
			eventually(t, "the follow-up window", func() bool {
				r.s.mu.Lock()
				defer r.s.mu.Unlock()
				return r.s.ex != nil && r.s.ex.followUpTimer != nil
			})
			said := asked
			if tc.after {
				said = time.Now()
			}
			r.s.OnTranscript(timedResult(tc.text, tc.start, true, from(said)))
			closed := func() bool { _, _, closed := sess.state(); return closed }
			if tc.stayOpen {
				never(t, "the exchange ended during the person's turn", 500*time.Millisecond, closed)
				return
			}
			eventually(t, "the exchange to end FollowUpWait after the answer", closed)
		})
	}
}

// TestTheQuestionsLateFinalAfterATextToSpeechAnswerAsksNothing fails if the
// question's final, sent late, once text to speech has answered the turn
// from its interim, is read in the follow-up window as the asker's next turn
// and asks the Concierge the same question again, with its wake phrase or
// without (Sol's review of #133 at f398ca8). The control, a follow-up said
// after the answer, is the next turn.
func TestTheQuestionsLateFinalAfterATextToSpeechAnswerAsksNothing(t *testing.T) {
	for _, kind := range []string{"control: a new follow-up", "the late final with its wake phrase", "the late final without it"} {
		t.Run(kind, func(t *testing.T) {
			c := newFakeConcierge("The issue is ready.")
			r := newRigWith(t, c, func(cfg *SupervisorConfig, _ *rig) { cfg.FollowUpWait = time.Minute })
			question := "Wispr, what is the status of INFRA ten twenty seven?"
			said := time.Now().Add(-5 * time.Second)
			at := func(i int) time.Time { return said.Add(time.Duration(i) * 100 * time.Millisecond) }
			r.s.OnTranscript(timedResult(question, 10, false, at))
			sess := r.waitSession()
			r.waitTurn() // at TurnWait, from the interim
			waitSaid(t, sess)
			r.speaker.finish(audio.InputVoice)
			eventually(t, "the answer's follow-up window", func() bool {
				r.s.mu.Lock()
				defer r.s.mu.Unlock()
				return r.s.ex != nil && r.s.ex.followUp
			})
			if kind == "control: a new follow-up" {
				r.heard("And who holds it?", true, true)
				if got := r.waitTurn(); got != "And who holds it?" {
					t.Fatalf("control: the follow-up's turn is %q", got)
				}
				return
			}
			if kind == "the late final without it" {
				question = "What is the status of INFRA ten twenty seven?"
			}
			r.s.OnTranscript(timedResult(question, 10, true, at))
			select {
			case got := <-c.asked:
				t.Fatalf("the question's late final asked the Concierge again: %q", got)
			case <-time.After(200 * time.Millisecond):
			}
		})
	}
}

// TestAWordAtTheVoicesFirstAudioIsNoTalkOver fails if a word said at the
// very moment the voice's first audio reached the speaker counts as talk-over:
// only a word said after it counts, with no slack either way. The control
// says the same words one nanosecond later, and they stop the voice (Sol's
// review of #133 at f398ca8 found the edge untested).
func TestAWordAtTheVoicesFirstAudioIsNoTalkOver(t *testing.T) {
	for _, delta := range []time.Duration{0, time.Nanosecond} {
		t.Run(delta.String(), func(t *testing.T) {
			r := newLongFollowUpDrivingRig(t)
			r.heard("Wispr, what is happening?", true, true)
			speakAs(t, r.waitSession(), "The issue is in review.")
			started := waitSpeechAfter(t, r, time.Time{})
			first := started.Add(delta)
			time.Sleep(time.Until(first.Add(250 * time.Millisecond)))
			r.s.OnTranscript(timedResult("Hold on.", 200, true, func(i int) time.Time { return first.Add(time.Duration(i) * 200 * time.Millisecond) }))
			if delta == 0 {
				never(t, "a word at the first audio counted as talk-over", 100*time.Millisecond, func() bool { return voiceCutAfter(r, started) })
				return
			}
			eventually(t, "words just after the first audio to stop the voice", func() bool { return voiceCutAfter(r, started) })
		})
	}
}

// TestALateFinalDoesNotOverwriteAFollowUp fails if the question's final,
// sent late, replaces a follow-up the room has begun since the driving voice
// answered: the answer tool and the side-talk check read the turn, which must
// be the follow-up.
func TestALateFinalDoesNotOverwriteAFollowUp(t *testing.T) {
	const followUpWords = "And who holds it?"
	r := newLongFollowUpDrivingRig(t)
	woke := time.Now().Add(-4 * time.Second)
	r.s.OnTranscript(timedResult("Wispr.", 192.93, true, from(woke)))
	sess := r.waitSession()
	asked := woke.Add(1600 * time.Millisecond)
	r.s.OnTranscript(timedResult(lateQuestion, 195.11, false, from(asked)))
	speakAs(t, sess, "INFRA-1027 is in review.")
	started := waitSpeechAfter(t, r, time.Time{})
	r.speaker.finish(audio.InputVoice) // the answer plays out: the follow-up window opens
	eventually(t, "the follow-up window", func() bool {
		r.s.mu.Lock()
		defer r.s.mu.Unlock()
		return r.s.ex != nil && r.s.ex.followUpTimer != nil
	})
	r.s.OnTranscript(timedResult(followUpWords, 199.0, false, from(started.Add(time.Second)))) // the follow-up begins
	r.s.OnTranscript(timedResult(lateQuestion, 195.11, true, from(asked)))                     // the question's final, late
	r.s.mu.Lock()
	turn, follow := r.s.turnOf(r.s.ex), r.s.ex.followUpTurn
	r.s.mu.Unlock()
	if turn != followUpWords || !follow {
		t.Fatalf("the turn is %q (a follow-up: %v), want the follow-up %q", turn, follow, followUpWords)
	}
}

// TestTheQuestionsOwnWordsDoNotStopTextToSpeech is the same for voice A,
// text to speech, whose turn the Supervisor hands over: Deepgram's endpoint
// never came, so the turn went at TurnWait with the question's interim, and
// its final came while the answer played. It fails if that final stops the
// answer.
func TestTheQuestionsOwnWordsDoNotStopTextToSpeech(t *testing.T) {
	r := newRig(t, newFakeConcierge("INFRA-1027 is in review."))
	woke := time.Now().Add(-4 * time.Second)
	r.s.OnTranscript(timedResult("Wispr.", 192.93, true, from(woke)))
	sess := r.waitSession()
	asked := woke.Add(1600 * time.Millisecond)
	r.s.OnTranscript(timedResult(lateQuestion, 195.11, false, from(asked)))
	r.waitTurn()
	waitSaid(t, sess)
	started := waitSpeechAfter(t, r, time.Time{})
	r.s.OnTranscript(timedResult(lateQuestion, 195.11, true, from(asked)))
	never(t, "the question's own final stopped the answer", 400*time.Millisecond, func() bool { return voiceCutAfter(r, started) })
}

// TestALateResultOfTheCallIsNoNewCall fails if the final of the call the
// voice answers, "Wispr, what's …" in one result, stops the voice when
// Deepgram's final moved its words from where the interim put them: over a
// driving voice it was taken as a barge-in with a call, and over text to
// speech as a new call that ended the answer. A call said before the voice
// spoke is no new call, however far its result moved. Its calls were all said
// before the voice began, so they cannot tell whether a final that moved the
// wake word by exactly 0.3 s is the same call: the moved words still come
// before the voice. TestAWakeWordAFinalMovedAtTheEdgeIsNoNewCall moves the
// wake word past the voice's start, and fails if that edge is not included
// (Sol's review of #133 at ab139c8).
func TestALateResultOfTheCallIsNoNewCall(t *testing.T) {
	const call = "Wispr, what's the status of INFRA ten twenty seven?"
	for _, voice := range []struct {
		name string
		rig  func(t *testing.T) *rig
		// speak makes the voice speak its answer to the call.
		speak func(t *testing.T, r *rig, sess *fakeSession)
	}{
		{"a driving voice", newLongFollowUpDrivingRig, func(t *testing.T, _ *rig, sess *fakeSession) { speakAs(t, sess, "Reading INFRA-1027.") }},
		{"text to speech", func(t *testing.T) *rig { return newRig(t, newFakeConcierge("INFRA-1027 is in review.")) }, func(t *testing.T, r *rig, sess *fakeSession) {
			r.waitTurn() // at TurnWait, from the interim: the endpoint never came
			waitSaid(t, sess)
		}},
	} {
		for _, moved := range []float64{0.1, 0.3, 0.5} {
			t.Run(fmt.Sprintf("%s, moved %.1f s", voice.name, moved), func(t *testing.T) {
				r := voice.rig(t)
				said := time.Now().Add(-3 * time.Second)
				r.s.OnTranscript(timedResult(call, 0, false, from(said)))
				sess := r.waitSession()
				voice.speak(t, r, sess)
				started := waitSpeechAfter(t, r, time.Time{})
				r.s.OnTranscript(timedResult(call, moved, true, from(said.Add(time.Duration(moved*float64(time.Second))))))
				never(t, "the call's own final stopped the voice", 400*time.Millisecond, func() bool { return voiceCutAfter(r, started) })
			})
		}
	}
}

// TestAFollowUpTheVoiceAnswersAtOnceIsNoTalkOver fails if a question the room
// asks after the driving voice's answer ended, which the voice answers less
// than a second later, stops that answer when Deepgram sends the question
// late: it was said while the voice was silent.
func TestAFollowUpTheVoiceAnswersAtOnceIsNoTalkOver(t *testing.T) {
	r := newLongFollowUpDrivingRig(t)
	r.heard("Wispr, what's the status of INFRA-1027?", true, true)
	sess := r.waitSession()
	speakAs(t, sess, "INFRA-1027 is in review since this morning.")
	first := waitSpeechAfter(t, r, time.Time{})
	r.speaker.finish(audio.InputVoice) // the answer ends by itself
	eventually(t, "the answer's end", func() bool {
		r.s.mu.Lock()
		defer r.s.mu.Unlock()
		return !r.s.voiceSpeaking
	})
	asked := from(time.Now().Add(50 * time.Millisecond)) // "Who signed off?"
	time.Sleep(time.Until(asked(2)) + 50*time.Millisecond)
	speakAs(t, sess, "Ada did.") // the voice answers at once
	second := waitSpeechAfter(t, r, first)
	if !asked(2).Before(second) {
		t.Fatalf("control: the question, said by %v, comes after its answer started at %v", asked(2), second)
	}
	r.s.OnTranscript(timedResult("Who signed off?", 210, true, asked)) // its late final
	never(t, "the follow-up's own final stopped its answer", 400*time.Millisecond, func() bool { return voiceCutAfter(r, second) })
}

// TestTheCallsInterruptionStartsANewTurn fails if talk-over that Deepgram
// sent in one result with the call cuts the driving voice but leaves the turn
// where it was: the interruption's own late final, after the voice answered
// it, was then taken as a new turn, and closed the follow-up window. It also
// fails if the interruption's words are not the turn once it cut the voice
// (Sol's review of #133 at aaa8e0f found the window alone checked).
func TestTheCallsInterruptionStartsANewTurn(t *testing.T) {
	const call = "Wispr, what's the status of INFRA-1027?"
	r := newDrivingRig(t, time.Minute) // FollowUpWait 200 ms
	said := time.Now().Add(-2 * time.Second)
	r.s.OnTranscript(timedResult(call, 0, false, from(said)))
	sess := r.waitSession()
	speakAs(t, sess, "INFRA-1027 is in review since this morning.")
	first := waitSpeechAfter(t, r, time.Time{})
	over := from(first.Add(100 * time.Millisecond)) // "Wait, who signed off?"
	time.Sleep(time.Until(over(3)) + 50*time.Millisecond)
	tr := transcriptOf(call+" Wait, who signed off?", 0, false, false, 0)
	for i := range tr.Words { // the call's six words, then the interruption's four
		tr.Words[i].At = said.Add(time.Duration(i) * 200 * time.Millisecond)
		if i >= 6 {
			tr.Words[i].At = over(i - 6)
		}
	}
	r.s.OnTranscript(&tr)
	eventually(t, "the interruption to cut the voice", func() bool { return voiceCutAfter(r, first) })
	r.s.mu.Lock()
	turn := r.s.turnOf(r.s.ex)
	r.s.mu.Unlock()
	if turn != "what's the status of INFRA-1027? Wait, who signed off?" {
		t.Fatalf("the turn is %q, want the call with its interruption", turn)
	}
	speakAs(t, sess, "Ada did.") // the voice answers the interruption
	waitSpeechAfter(t, r, first)
	r.speaker.finish(audio.InputVoice) // the follow-up window opens
	eventually(t, "the follow-up window", func() bool {
		r.s.mu.Lock()
		defer r.s.mu.Unlock()
		return r.s.ex != nil && r.s.ex.followUpTimer != nil
	})
	r.s.OnTranscript(timedResult("Wait, who signed off?", 1.0, true, over)) // the interruption's late final
	eventually(t, "the exchange to end FollowUpWait after the answer", func() bool { _, _, closed := sess.state(); return closed })
}

// TestTheEndOfSpeechTheCallCutIsNotTheNewVoices fails if the end of other
// speech, which a call cut as its exchange took the floor, counts as the end
// of that exchange's own driving voice's speech: it opened the follow-up
// window before the voice had said anything, and the exchange ended while
// the room waited for its answer.
func TestTheEndOfSpeechTheCallCutIsNotTheNewVoices(t *testing.T) {
	r := newDrivingRig(t, time.Minute) // FollowUpWait 200 ms
	if err := r.s.WithFloor(t.Context(), func() {
		go func() { _ = r.speaker.Begin(audio.InputVoice).Write(t.Context(), make([]byte, 960), ChimeFormat) }()
	}); err != nil {
		t.Fatal(err)
	}
	r.waitSpeaking() // other speech, as POST /api/tts/speak plays
	r.heard("Wispr, what's the status of INFRA-1027?", true, true)
	sess := r.waitSession()
	never(t, "the exchange ended before its voice spoke", 500*time.Millisecond, func() bool { _, _, closed := sess.state(); return closed })
}

// TestASpeechThatStartedBeforeTheCallIsNotItsVoices fails if other speech,
// which started before a call took the floor but whose start the Supervisor
// heard only after, as an event still queued, counts as the call's own
// voice's: its end, the cut the call made, opened the follow-up window before
// the voice had said anything, and the exchange ended.
func TestASpeechThatStartedBeforeTheCallIsNotItsVoices(t *testing.T) {
	r := newDrivingRig(t, time.Minute) // FollowUpWait 200 ms
	earlier := time.Now()              // the other speech's first audio went to the speaker
	time.Sleep(20 * time.Millisecond)
	r.heard("Wispr, what's the status of INFRA-1027?", true, true) // the call takes the floor
	sess := r.waitSession()
	r.speaker.emit(audio.SpeechEvent{Input: audio.InputVoice, Started: true, At: earlier})                              // its start, queued
	r.speaker.emit(audio.SpeechEvent{Input: audio.InputVoice, At: earlier.Add(10 * time.Millisecond), Cancelled: true}) // the call cut it
	never(t, "the exchange ended before its voice spoke", 500*time.Millisecond, func() bool { _, _, closed := sess.state(); return closed })
}

// TestATalkOverReadInAPauseDoesNotStopTheVoice fails if two words said over a
// driving voice, whose final comes only once its audio has paused, stop the
// vendor or cancel the speaker, or leave its next answer without its
// follow-up window. The Supervisor stops a voice only while it plays: a
// talk-over recognized after the voice went quiet does not interrupt the
// vendor's turn, the limit this change states. An earlier rule that stopped
// it in the pause left the next answer without its follow-up window (Sol's
// review of #133 at ab139c8). The control reads the same words while the
// voice plays, and they stop it both ways.
func TestATalkOverReadInAPauseDoesNotStopTheVoice(t *testing.T) {
	for _, pause := range []bool{false, true} {
		t.Run(fmt.Sprintf("read in a pause %v", pause), func(t *testing.T) {
			r := newDrivingRig(t, time.Minute) // FollowUpWait 200 ms
			r.heard("Wispr, what's the status of INFRA-1027?", true, true)
			sess := r.waitSession()
			speakAs(t, sess, "INFRA-1027 is in review.")
			first := waitSpeechAfter(t, r, time.Time{})
			said := from(first.Add(30 * time.Millisecond)) // "Hold on", over the speech
			time.Sleep(time.Until(said(1)) + 20*time.Millisecond)
			if pause {
				r.speaker.finish(audio.InputVoice) // its audio pauses
				r.speaker.FlushSpeech()
			}
			r.s.OnTranscript(timedResult("Hold on.", 200, true, said))
			interrupted := func() bool { i, _, _ := sess.state(); return !i.IsZero() }
			if !pause {
				eventually(t, "the talk-over to stop the voice", interrupted)
				if !voiceCutAfter(r, first) {
					t.Fatal("control: the talk-over did not cancel the speaker")
				}
				return
			}
			never(t, "a talk-over read in the pause stopped the voice", 200*time.Millisecond, func() bool { return interrupted() || voiceCutAfter(r, first) })
			speakAs(t, sess, "Ada has signed off.") // the vendor goes on
			waitSpeechAfter(t, r, first)
			r.speaker.finish(audio.InputVoice)
			eventually(t, "the next answer's follow-up window", func() bool {
				r.s.mu.Lock()
				defer r.s.mu.Unlock()
				return r.s.ex != nil && r.s.ex.followUpTimer != nil
			})
			eventually(t, "the exchange to end after its follow-up window", func() bool { _, _, closed := sess.state(); return closed })
		})
	}
}

// TestANewCallSaidOverTheVoiceIsACallWhenReadInAPause fails if a call,
// "Wispr, who holds it?", said over a driving voice and read once its audio
// has paused, is not the asker's new call: its words the turn, not a
// follow-up, and the follow-up window closed. It does not stop the vendor,
// as the voice is already quiet. The control reads it while the voice plays,
// and it stops the voice, a barge-in with a call.
func TestANewCallSaidOverTheVoiceIsACallWhenReadInAPause(t *testing.T) {
	for _, pause := range []bool{false, true} {
		t.Run(fmt.Sprintf("read in a pause %v", pause), func(t *testing.T) {
			r := newLongFollowUpDrivingRig(t)
			r.s.OnTranscript(timedResult("Wispr, what's the status of INFRA-1027?", 10, true, from(time.Now().Add(-3*time.Second))))
			sess := r.waitSession()
			speakAs(t, sess, "INFRA-1027 is in review since this morning.")
			started := waitSpeechAfter(t, r, time.Time{})
			call := started.Add(50 * time.Millisecond)
			time.Sleep(time.Until(call) + 50*time.Millisecond)
			if pause {
				r.speaker.finish(audio.InputVoice)
				r.speaker.FlushSpeech()
			}
			r.s.OnTranscript(timedResult("Wispr, who holds it?", 200, true, from(call)))
			interrupted := func() bool { i, _, _ := sess.state(); return !i.IsZero() }
			if !pause {
				eventually(t, "the call to stop the vendor", interrupted)
				return
			}
			never(t, "a call read in the pause stopped the quiet voice", 200*time.Millisecond, interrupted)
			r.s.mu.Lock()
			turn, follow, window := r.s.turnOf(r.s.ex), r.s.ex.followUpTurn, r.s.ex.followUpTimer != nil
			r.s.mu.Unlock()
			if turn != "who holds it?" || follow || window {
				t.Fatalf("the turn is %q (a follow-up: %v, the window open: %v), want the new call's %q", turn, follow, window, "who holds it?")
			}
		})
	}
}

// TestAnOldEchoDoesNotStopTheVoicesNextSpeech fails if the voice's own words,
// heard back during one speech and read only during its next, after
// ownWordsKept, stop that next speech: starting the next line forgot the
// earlier line's words while the exchange still kept the earlier speech's
// span, so the echo passed as talk-over (Sol's review of #133 at aaa8e0f).
// The control reads other words said at the same moment, which do talk over
// the earlier speech, and they stop the voice.
func TestAnOldEchoDoesNotStopTheVoicesNextSpeech(t *testing.T) {
	for _, own := range []bool{true, false} {
		t.Run(fmt.Sprintf("its own words %v", own), func(t *testing.T) {
			kept := ownWordsKept
			ownWordsKept = 300 * time.Millisecond
			t.Cleanup(func() { ownWordsKept = kept }) // after the rig has stopped
			r := newLongFollowUpDrivingRig(t)
			r.heard("Wispr, what is the status?", true, true)
			sess := r.waitSession()
			speakAs(t, sess, "Release progress.")
			first := waitSpeechAfter(t, r, time.Time{})
			said := from(first.Add(30 * time.Millisecond))
			time.Sleep(time.Until(said(1)) + 30*time.Millisecond)
			r.speaker.finish(audio.InputVoice)
			r.speaker.FlushSpeech()
			time.Sleep(ownWordsKept + 100*time.Millisecond)
			speakAs(t, sess, "Ada holds it.")
			second := waitSpeechAfter(t, r, first)
			text := "Release progress."
			if !own {
				text = "Hold on."
			}
			r.s.OnTranscript(timedResult(text, 200, true, said))
			if !own {
				eventually(t, "control: words said over the earlier speech to stop the voice", func() bool { return voiceCutAfter(r, second) })
				return
			}
			never(t, "the voice's old echo stopped its next speech", 250*time.Millisecond, func() bool { return voiceCutAfter(r, second) })
		})
	}
}

// TestALateEchoInAPauseIsNotTheAskersTurn fails if the voice's own words,
// heard back while it played, are taken as the asker's next turn when their
// final comes in a pause of its audio, more than echoHold after its speech
// ended: the own-word filter went by when the result came, not when its words
// were captured (Sol's review of #133 at 99f652f). The control reads words
// the voice did not say, captured while it played and read in the same
// pause: they are the asker's next turn, and close the follow-up window.
func TestALateEchoInAPauseIsNotTheAskersTurn(t *testing.T) {
	for _, own := range []bool{false, true} {
		t.Run(fmt.Sprintf("its own words %v", own), func(t *testing.T) {
			r := newLongFollowUpDrivingRig(t)
			r.heard("Wispr, what's the status of INFRA-1027?", true, true)
			sess := r.waitSession()
			spoken := "The issue is in review."
			if !own {
				spoken = "Ada holds it."
			}
			speakAs(t, sess, spoken)
			started := waitSpeechAfter(t, r, time.Time{})
			said := from(started.Add(30 * time.Millisecond)) // "in review", over the speech
			time.Sleep(time.Until(said(1)) + 20*time.Millisecond)
			r.speaker.finish(audio.InputVoice) // its audio pauses
			r.speaker.FlushSpeech()
			window := func() bool { r.s.mu.Lock(); defer r.s.mu.Unlock(); return r.s.ex != nil && r.s.ex.followUpTimer != nil }
			eventually(t, "the follow-up window", window)
			time.Sleep(echoHold + 50*time.Millisecond)
			r.s.mu.Lock()
			before := r.s.turnOf(r.s.ex)
			r.s.mu.Unlock()
			r.s.OnTranscript(timedResult("in review.", 200, true, said))
			r.s.mu.Lock()
			turn := r.s.turnOf(r.s.ex)
			r.s.mu.Unlock()
			if own {
				if turn != before || !window() {
					t.Fatalf("the voice's own echo made the turn %q (was %q; the window open: %v)", turn, before, window())
				}
				return
			}
			if turn != "in review." || window() {
				t.Fatalf("control: the turn is %q (the window open: %v), want the words said over the voice", turn, window())
			}
		})
	}
}

// TestATalkOverBeforeItsStartEventIsDeliveredStopsTheVoice fails if two words
// said over the voice do not stop it when their result is read before the
// speaker has delivered the speech's start: the start was queued, not yet
// delivered, and the Supervisor saw no speech (Sol's review of #133 at
// b6eef5c). The Supervisor reads the speaker's queued events first.
func TestATalkOverBeforeItsStartEventIsDeliveredStopsTheVoice(t *testing.T) {
	r := newLongFollowUpDrivingRig(t)
	r.heard("Wispr, what's the status of INFRA-1027?", true, true)
	sess := r.waitSession()
	release := r.speaker.holdEvents()
	t.Cleanup(release)
	speakAs(t, sess, "INFRA-1027 is in review.") // its audio is taken; its start waits for delivery
	said := from(time.Now().Add(50 * time.Millisecond))
	time.Sleep(time.Until(said(1)) + 50*time.Millisecond)
	r.s.OnTranscript(timedResult("Hold on.", 200, true, said))
	eventually(t, "the vendor to be interrupted", func() bool { i, _, _ := sess.state(); return !i.IsZero() })
}

// TestAFollowUpAnsweredBeforeItsTranscriptIsNoNewTurn fails if a follow-up
// that the driving voice answered from its own audio of the room, before
// Deepgram's result for it came, counts as a new turn once that result comes
// after the answer: it closed the follow-up window, and the exchange stayed
// open for IdleWait.
func TestAFollowUpAnsweredBeforeItsTranscriptIsNoNewTurn(t *testing.T) {
	// FollowUpWait outlasts the follow-up said in it, and is shorter than
	// eventually's wait.
	r := newRigWith(t, newFakeConcierge(), func(cfg *SupervisorConfig, r *rig) {
		v := drivingVoice{r.voice}
		cfg.Voice = func() (Voice, error) { return v, nil }
		cfg.IdleWait = time.Minute
		cfg.FollowUpWait = time.Second
	})
	r.heard("Wispr, what's the status of INFRA-1027?", true, true)
	sess := r.waitSession()
	window := func() bool {
		r.s.mu.Lock()
		defer r.s.mu.Unlock()
		return r.s.ex != nil && r.s.ex.followUpTimer != nil
	}
	speakAs(t, sess, "INFRA-1027 is in review since this morning.")
	first := waitSpeechAfter(t, r, time.Time{})
	r.speaker.finish(audio.InputVoice)
	eventually(t, "the first answer's follow-up window", window)
	asked := from(time.Now()) // "Who signed off?", in the window; Deepgram is slow
	time.Sleep(time.Until(asked(2)) + 50*time.Millisecond)
	speakAs(t, sess, "Ada did.") // the voice answers it from the room's audio
	waitSpeechAfter(t, r, first)
	r.speaker.finish(audio.InputVoice)
	eventually(t, "the second answer's follow-up window", window)
	r.s.OnTranscript(timedResult("Who signed off?", 210, true, asked)) // its final, after the answer
	eventually(t, "the exchange to end FollowUpWait after the second answer", func() bool { _, _, closed := sess.state(); return closed })
}

// TestAWakeWordAFinalMovedAtTheEdgeIsNoNewCall fails if a final that moved the
// wake word exactly 0.3 s, at a point of the stream where the subtraction is
// not exact (10.3 - 10), is taken as a new call over the driving voice and
// cuts it: its words, moved later, fall after the voice began. The move is
// the same on the capture clock as on the socket's.
func TestAWakeWordAFinalMovedAtTheEdgeIsNoNewCall(t *testing.T) {
	r := newLongFollowUpDrivingRig(t)
	said := time.Now()
	r.s.OnTranscript(timedResult("Wispr.", 10, false, from(said)))
	sess := r.waitSession()
	speakAs(t, sess, "Yes?")
	started := waitSpeechAfter(t, r, time.Time{})
	moved := said.Add(300 * time.Millisecond) // Deepgram's final moved the word 0.3 s
	if !moved.After(started) {
		t.Fatalf("control: the moved word, at %v, is before the voice began at %v", moved, started)
	}
	time.Sleep(time.Until(moved) + 10*time.Millisecond)
	r.s.OnTranscript(timedResult("Wispr.", 10.3, true, from(moved)))
	never(t, "the call's own final cut the voice", 400*time.Millisecond, func() bool { return voiceCutAfter(r, started) })
}

// TestANewCallOnANewSocketIsANewCall fails if a call said over the driving
// voice is taken for the call it answers because a new socket restarted
// Deepgram's stream, and the new call's word starts where the old one's did
// (Sol's review of #133 at 3c1700b): it must cut the voice, as a barge-in
// with a call. The control, the same call at another place in the stream,
// cuts it either way.
func TestANewCallOnANewSocketIsANewCall(t *testing.T) {
	for _, at := range []float64{10, 200} {
		t.Run(fmt.Sprintf("the new call at %v s", at), func(t *testing.T) {
			r := newLongFollowUpDrivingRig(t)
			r.s.OnTranscript(timedResult("Wispr, what's the status of INFRA-1027?", 10, true, from(time.Now().Add(-2*time.Second))))
			sess := r.waitSession()
			speakAs(t, sess, "INFRA-1027 is in review since this morning.")
			started := waitSpeechAfter(t, r, time.Time{})
			call := started.Add(100 * time.Millisecond)
			time.Sleep(time.Until(call) + 20*time.Millisecond)
			r.s.OnTranscript(timedResult("Wispr.", at, true, from(call)))
			eventually(t, "the new call to cut the voice", func() bool { return voiceCutAfter(r, started) })
		})
	}
}

// TestTalkOverStillStopsTheVoice fails if words a person says over the voice,
// after its speech started, no longer stop it, or if the tail of the question
// in the same result counts toward the two words a barge-in takes. Deepgram
// can send the talk-over in one result with the call the voice answers, which
// repeats the call's wake phrase.
func TestTalkOverStillStopsTheVoice(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tts   bool // text to speech, else a driving voice
		text  string
		start float64 // the result's first word on the socket; the call starts at 0
		after int     // the words, from the end, said after the voice started
		stops bool
	}{
		{"talk-over", false, "wait, who holds it?", 200, 4, true},
		{"the question's tail, then talk-over", false, "INFRA ten twenty seven? Wait, who holds it?", 200, 4, true},
		{"the question's tail, then one word", false, "INFRA ten twenty seven? Wait,", 200, 1, false},
		{"the call, then talk-over, in one result", false, "Wispr, what's the status of INFRA-1027? Hold on, who holds it?", 0, 5, true},
		{"the call, then one word, in one result", false, "Wispr, what's the status of INFRA-1027? Hold", 0, 1, false},
		{"text to speech: talk-over", true, "wait, who holds it?", 200, 4, true},
		{"text to speech: the call, then talk-over, in one result", true, "Wispr, what's the status of INFRA-1027? Hold on, who holds it?", 0, 5, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var r *rig
			if tc.tts {
				r = newRig(t, newFakeConcierge("INFRA-1027 is in review since this morning."))
				waitSaid(t, r.ask("what's the status of INFRA-1027?"))
			} else {
				r = newLongFollowUpDrivingRig(t)
				r.heard("Wispr, what's the status of INFRA-1027?", true, true)
				speakAs(t, r.waitSession(), "INFRA-1027 is in review since this morning.")
			}
			started := waitSpeechAfter(t, r, time.Time{})
			tr := transcriptOf(tc.text, tc.start, false, false, 0)
			before := len(tr.Words) - tc.after
			for i := range tr.Words {
				// The question's words end 0.4 s before the voice starts;
				// the talk-over starts 0.3 s after it.
				tr.Words[i].At = started.Add(time.Duration(i-before)*200*time.Millisecond - 200*time.Millisecond)
				if i >= before {
					tr.Words[i].At = started.Add(300*time.Millisecond + time.Duration(i-before)*200*time.Millisecond)
				}
			}
			r.s.OnTranscript(&tr)
			if tc.stops {
				eventually(t, "the talk-over to stop the voice", func() bool { return voiceCutAfter(r, started) })
				return
			}
			never(t, "one word over the voice stopped it", 400*time.Millisecond, func() bool { return voiceCutAfter(r, started) })
		})
	}
}

// TestTalkOverIsSpacedByWhenItWasSaid fails if two words said 0.8 s apart
// over the voice stop it because their socket starts were 0.2 s apart, as
// when the send queue dropped audio between them (Sol's review of #133 at
// 55bdf53). The control, the same words said 0.2 s apart, stops it.
func TestTalkOverIsSpacedByWhenItWasSaid(t *testing.T) {
	for _, tc := range []struct {
		name  string
		apart time.Duration
		stops bool
	}{
		{"0.8 s apart, 0.2 s on the socket", 800 * time.Millisecond, false},
		{"control: 0.2 s apart", 200 * time.Millisecond, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newLongFollowUpDrivingRig(t)
			r.heard("Wispr, what's the status of INFRA-1027?", true, true)
			speakAs(t, r.waitSession(), "INFRA-1027 is in review since this morning.")
			started := waitSpeechAfter(t, r, time.Time{})
			first := started.Add(100 * time.Millisecond)
			time.Sleep(time.Until(first.Add(tc.apart)) + 50*time.Millisecond)
			r.s.OnTranscript(timedResult("Hold on.", 200, false, func(i int) time.Time { return first.Add(time.Duration(i) * tc.apart) }))
			if tc.stops {
				eventually(t, "the talk-over to stop the voice", func() bool { return voiceCutAfter(r, started) })
				return
			}
			never(t, "words said 0.8 s apart stopped the voice", 400*time.Millisecond, func() bool { return voiceCutAfter(r, started) })
		})
	}
}

// TestTalkOverSplitAcrossFinalsStopsTheVoice fails if two words said over
// the voice 0.2 s apart do not stop it when Deepgram sends each in a final of
// its own (Sol's review of #133 at 55bdf53), or if an interim and the final
// of the same one word count as two.
func TestTalkOverSplitAcrossFinalsStopsTheVoice(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stops bool
	}{
		{"hold, then on, in two finals", true},
		{"control: hold, as an interim and its final", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newLongFollowUpDrivingRig(t)
			r.heard("Wispr, what's the status of INFRA-1027?", true, true)
			speakAs(t, r.waitSession(), "INFRA-1027 is in review since this morning.")
			started := waitSpeechAfter(t, r, time.Time{})
			hold := started.Add(100 * time.Millisecond)
			time.Sleep(time.Until(hold.Add(200*time.Millisecond)) + 50*time.Millisecond)
			if tc.stops {
				r.s.OnTranscript(timedResult("Hold.", 200, true, func(int) time.Time { return hold }))
				r.s.OnTranscript(timedResult("on.", 200.2, true, func(int) time.Time { return hold.Add(200 * time.Millisecond) }))
				eventually(t, "the talk-over to stop the voice", func() bool { return voiceCutAfter(r, started) })
				return
			}
			r.s.OnTranscript(timedResult("Hold", 200, false, func(int) time.Time { return hold }))
			r.s.OnTranscript(timedResult("Hold.", 200, true, func(int) time.Time { return hold }))
			r.s.OnTranscript(timedResult("Hold.", 200, false, func(int) time.Time { return hold }))
			never(t, "one word stopped the voice", 400*time.Millisecond, func() bool { return voiceCutAfter(r, started) })
		})
	}
}

// TestTalkOverPairsByCaptureTimeAcrossResults fails if the pairing of a
// final's last talk-over word with the next result's first goes by
// Deepgram's stream instead of the capture clock (Sol's review of #133 at
// 2f101fa): one short word sent again in a later result counted as two, and
// two words 0.2 s apart did not pair when a new socket restarted Deepgram's
// stream between their results.
func TestTalkOverPairsByCaptureTimeAcrossResults(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stops bool
		// first and second are each result's word, its socket start and
		// length, and its capture time after hold.
		first, second       string
		firstAt, secondAt   float64
		firstLen, secondLen float64
		secondFinal         bool
		secondAfter         time.Duration
	}{
		{"a 40 ms word, then the same word again", false, "It.", "It.", 200, 200, 0.04, 0.04, false, 0},
		{"control: a 40 ms word, then the next", true, "It.", "on.", 200, 200.2, 0.04, 0.15, true, 200 * time.Millisecond},
		{"hold, then on, on a new socket", true, "Hold.", "on.", 200, 0, 0.15, 0.15, true, 200 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newLongFollowUpDrivingRig(t)
			r.heard("Wispr, what's the status of INFRA-1027?", true, true)
			speakAs(t, r.waitSession(), "INFRA-1027 is in review since this morning.")
			started := waitSpeechAfter(t, r, time.Time{})
			hold := started.Add(100 * time.Millisecond)
			time.Sleep(time.Until(hold.Add(tc.secondAfter)) + 50*time.Millisecond)
			result := func(text string, start, length float64, final bool, at time.Time) *Transcript {
				return &Transcript{Text: text, IsFinal: final, GateOpen: true, At: time.Now(), Words: []Word{{Text: text, Start: start, End: start + length, At: at}}}
			}
			r.s.OnTranscript(result(tc.first, tc.firstAt, tc.firstLen, true, hold))
			r.s.OnTranscript(result(tc.second, tc.secondAt, tc.secondLen, tc.secondFinal, hold.Add(tc.secondAfter)))
			if tc.stops {
				eventually(t, "the talk-over to stop the voice", func() bool { return voiceCutAfter(r, started) })
				return
			}
			never(t, "one word stopped the voice", 400*time.Millisecond, func() bool { return voiceCutAfter(r, started) })
		})
	}
}

// TestATalkOverWordWhoseFinalCameInAPausePairs fails if a word said over the
// voice, whose final came while its audio paused, does not pair with the
// next word said over its next speech within 0.5 s (Sol's review of #133 at
// 2f101fa): both were said while it played.
func TestATalkOverWordWhoseFinalCameInAPausePairs(t *testing.T) {
	r := newLongFollowUpDrivingRig(t)
	r.heard("Wispr, what's the status of INFRA-1027?", true, true)
	sess := r.waitSession()
	speakAs(t, sess, "INFRA-1027 is in review.")
	first := waitSpeechAfter(t, r, time.Time{})
	time.Sleep(50 * time.Millisecond)
	hold := time.Now().Add(-20 * time.Millisecond) // said while the first speech plays
	r.speaker.finish(audio.InputVoice)             // its audio pauses
	eventually(t, "the pause", func() bool { r.s.mu.Lock(); defer r.s.mu.Unlock(); return !r.s.voiceSpeaking })
	r.s.OnTranscript(timedResult("Hold.", 200, true, func(int) time.Time { return hold })) // its final, in the pause
	speakAs(t, sess, "Ada holds it.")                                                      // no word of it is the talk-over's
	second := waitSpeechAfter(t, r, first)
	// "on" starts after "hold" ended (0.15 s long), during the next speech.
	on := hold.Add(200 * time.Millisecond)
	if !on.After(second) {
		on = second.Add(10 * time.Millisecond)
	}
	if on.Sub(hold) > 500*time.Millisecond {
		t.Fatalf("control: the next speech started %v after hold, too late for a pair", second.Sub(hold))
	}
	time.Sleep(time.Until(on) + 20*time.Millisecond)
	r.s.OnTranscript(timedResult("on.", 200.3, true, func(int) time.Time { return on }))
	eventually(t, "the talk-over to stop the voice", func() bool { return voiceCutAfter(r, second) })
}

// TestTalkOverCountsAcrossAPauseInTheVoice fails if words a person said over
// the voice stop counting because its audio paused and started again before
// their result came, as a vendor's audio can stall for longer than the 100 ms
// that ends a speech at the speaker: the person still talked over it. The
// control fails if the words of an interruption that cut the voice then cut
// the voice's next speech, which answers them.
func TestTalkOverCountsAcrossAPauseInTheVoice(t *testing.T) {
	t.Run("a pause in the voice's audio", func(t *testing.T) {
		r := newLongFollowUpDrivingRig(t)
		r.heard("Wispr, what's the status of INFRA-1027?", true, true)
		sess := r.waitSession()
		speakAs(t, sess, "INFRA-1027 is in review since this morning.")
		first := waitSpeechAfter(t, r, time.Time{})
		// "Wait, stop", said 0.1 s into the speech, before its audio stalls.
		said := from(first.Add(100 * time.Millisecond))
		time.Sleep(time.Until(said(1)) + 50*time.Millisecond)
		r.speaker.finish(audio.InputVoice) // the vendor's audio stalls
		time.Sleep(600 * time.Millisecond)
		speakAs(t, sess, "Its holder is the agent on it.")
		second := waitSpeechAfter(t, r, first)
		if !said(1).Before(second) {
			t.Fatalf("control: the words, said by %v, come after the speech started again at %v", said(1), second)
		}
		r.s.OnTranscript(timedResult("Wait, stop.", 200, false, said))
		eventually(t, "the talk-over to stop the voice", func() bool { return voiceCutAfter(r, first) })
	})
	t.Run("control: the interruption the voice now answers", func(t *testing.T) {
		r := newLongFollowUpDrivingRig(t)
		r.heard("Wispr, what's the status of INFRA-1027?", true, true)
		sess := r.waitSession()
		speakAs(t, sess, "INFRA-1027 is in review since this morning.")
		first := waitSpeechAfter(t, r, time.Time{})
		said := from(first.Add(100 * time.Millisecond))
		time.Sleep(time.Until(said(3)) + 50*time.Millisecond) // its result comes after its words
		r.s.OnTranscript(timedResult("wait, who holds it?", 200, false, said))
		eventually(t, "the interruption to stop the voice", func() bool { return voiceCutAfter(r, first) })
		cut, _ := r.speaker.cancelledAt(audio.InputVoice)
		speakAs(t, sess, "Its holder is the agent on it.") // the vendor answers it at once
		if second := waitSpeechAfter(t, r, first); !said(3).Before(second) {
			t.Fatalf("control: the interruption, said by %v, comes after its answer started at %v", said(3), second)
		}
		r.s.OnTranscript(timedResult("wait, who holds it?", 200, true, said)) // the interruption's late final
		never(t, "the interruption's own final stopped its answer", 400*time.Millisecond, func() bool { return voiceCutAfter(r, cut) })
	})
}

// TestTheVoicesOwnEchoDoesNotStopIt fails if the voice's own words, heard
// back through the room after its speech started, stop it, as GPT-Live's
// could: GPT-Live sends each fragment of its words just before their audio,
// and the Supervisor keeps them for the own-speech filter. The control,
// words the voice did not say heard at the same time, stops it, so the check
// can fail.
func TestTheVoicesOwnEchoDoesNotStopIt(t *testing.T) {
	for _, tc := range []struct {
		name  string
		said  string
		stops bool
	}{
		{"its own words, heard back", "INFRA-1027 is in review since this morning.", false},
		{"control: words it did not say", "Reading INFRA-1027.", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newLongFollowUpDrivingRig(t)
			r.heard("Wispr, what's the status of INFRA-1027?", true, true)
			sess := r.waitSession()
			speakAs(t, sess, tc.said)
			started := waitSpeechAfter(t, r, time.Time{})
			r.s.OnTranscript(timedResult("is in review since this morning.", 200, false, from(started.Add(300*time.Millisecond))))
			if tc.stops {
				eventually(t, "the control's words to stop the voice", func() bool { return voiceCutAfter(r, started) })
				return
			}
			never(t, "the voice's own echo stopped it", 400*time.Millisecond, func() bool { return voiceCutAfter(r, started) })
		})
	}
}

// TestAFinalReadInAPauseCarriesItsLastWord fails if a final of several words
// said over the voice, read while it is quiet, carries its second talk-over
// word to the voice's next speech instead of its last: the pair was found,
// and the words after it were not read (Sol's review of #133 at 0dfeaad).
// "Hold on please" said at +100, +300 and +500 ms of a speech, then "Wait" at
// +900 ms of the next, which began at +800 ms: "please" and "Wait" are 0.4 s
// apart and pair; "on" is 0.6 s from "Wait". The control's final is the one
// word "Please" at +500 ms.
func TestAFinalReadInAPauseCarriesItsLastWord(t *testing.T) {
	for _, multi := range []bool{false, true} {
		t.Run(fmt.Sprintf("several words %v", multi), func(t *testing.T) {
			base := time.Now().Add(-2 * time.Second)
			ex := &exchange{drives: true, played: []span{{from: base, to: base.Add(700 * time.Millisecond)}}}
			text, at := "Please.", base.Add(500*time.Millisecond)
			if multi {
				text, at = "Hold on please.", base.Add(100*time.Millisecond)
			}
			_, carried := bargesIn(timedResult(text, 200, true, from(at)), nil, ex, nil)
			if carried == nil {
				t.Fatal("the final carried no word said over the voice")
			}
			ex.played = append(ex.played, span{from: base.Add(800 * time.Millisecond)}) // its next speech
			if cut, _ := bargesIn(timedResult("Wait.", 201, true, from(base.Add(900*time.Millisecond))), nil, ex, carried); !cut {
				t.Fatalf("the final's last word, at +500ms, did not pair with \"Wait\" at +900ms: it carried the word at +%v", carried.at.Sub(base))
			}
		})
	}
}

// TestTheQuestionsFinalDoesNotReplaceAFollowUpTheVoiceAnswers fails if the
// original question's final, which Deepgram sent late, replaces the
// follow-up the room asked after the first answer, once the voice has begun
// to answer that follow-up: the voice's next speech made any late result
// complete the turn, though the question was said before that turn began
// (Sol's review of #133 at 0dfeaad). TestALateFinalDoesNotOverwriteAFollowUp
// reads the final before the second answer starts.
func TestTheQuestionsFinalDoesNotReplaceAFollowUpTheVoiceAnswers(t *testing.T) {
	const follow = "And who holds it?"
	r := newLongFollowUpDrivingRig(t)
	woke := time.Now().Add(-4 * time.Second)
	r.s.OnTranscript(timedResult("Wispr.", 192.93, true, from(woke)))
	sess := r.waitSession()
	asked := woke.Add(1600 * time.Millisecond)
	r.s.OnTranscript(timedResult(lateQuestion, 195.11, false, from(asked)))
	speakAs(t, sess, "INFRA-1027 is in review.")
	first := waitSpeechAfter(t, r, time.Time{})
	r.speaker.finish(audio.InputVoice)
	r.speaker.FlushSpeech()
	followAt := from(time.Now())
	time.Sleep(time.Until(followAt(3)) + 20*time.Millisecond)
	r.s.OnTranscript(timedResult(follow, 199, false, followAt))
	turn := func() string { r.s.mu.Lock(); defer r.s.mu.Unlock(); return r.s.turnOf(r.s.ex) }
	if got := turn(); got != follow {
		t.Fatalf("control: before the second answer, the turn is %q, want %q", got, follow)
	}
	speakAs(t, sess, "Ada holds it.")
	if second := waitSpeechAfter(t, r, first); !followAt(3).Before(second) {
		t.Fatal("control: the second answer began before the follow-up was said")
	}
	r.s.OnTranscript(timedResult(lateQuestion, 195.11, true, from(asked)))
	if got := turn(); got != follow {
		t.Fatalf("the question's late final made the turn %q, want the follow-up %q", got, follow)
	}
}

// TestANewCallSaidOverTheVoiceClosesTheWindowAfterAnInterim fails if a call,
// "Hey, Wispr, who holds it?", said over the voice and read once its speech
// ended, leaves the follow-up window that end opened, when an interim of its
// first word had begun the turn while the voice played: the exchange then
// ended at FollowUpWait though a call was made (Sol's review of #133 at
// 0dfeaad). TestANewCallSaidOverTheVoiceIsACallWhenReadInAPause has no such
// interim. A call closes the window (R10).
func TestANewCallSaidOverTheVoiceClosesTheWindowAfterAnInterim(t *testing.T) {
	r := newLongFollowUpDrivingRig(t)
	r.s.OnTranscript(timedResult("Wispr, what's the status of INFRA-1027?", 10, true, from(time.Now().Add(-3*time.Second))))
	sess := r.waitSession()
	speakAs(t, sess, "INFRA-1027 is in review.")
	started := waitSpeechAfter(t, r, time.Time{})
	callAt := from(started.Add(30 * time.Millisecond))
	time.Sleep(time.Until(callAt(0)) + 20*time.Millisecond)
	r.s.OnTranscript(timedResult("Hey", 200, false, callAt)) // one word cuts nothing
	if voiceCutAfter(r, started) {
		t.Fatal("control: one word cut the voice")
	}
	time.Sleep(time.Until(callAt(4).Add(150*time.Millisecond)) + 20*time.Millisecond)
	r.speaker.finish(audio.InputVoice)
	r.speaker.FlushSpeech()
	window := func() bool { r.s.mu.Lock(); defer r.s.mu.Unlock(); return r.s.ex != nil && r.s.ex.followUpTimer != nil }
	if !window() {
		t.Fatal("control: the speech's end did not open the follow-up window")
	}
	r.s.OnTranscript(timedResult("Hey, Wispr, who holds it?", 200, true, callAt))
	r.s.mu.Lock()
	turn, follow := r.s.turnOf(r.s.ex), r.s.ex.followUpTurn
	r.s.mu.Unlock()
	if turn != "who holds it?" || follow || window() {
		t.Fatalf("the call left the turn %q (a follow-up: %v, the window open: %v), want its words, no follow-up, the window closed", turn, follow, window())
	}
}

// TestAnUntimedWakeWordAFinalMovedAtTheEdgeIsNoNewCall fails if a final that
// moved an untimed wake word exactly 0.3 s on the socket, from 10 s to 10.3 s,
// is taken as a new call over the driving voice and cuts it: the edge is
// included on the socket's clock too. TestAWakeWordAFinalMovedAtTheEdgeIsNoNewCall
// times its words, so it reads only the capture clock's edge (Sol's review of
// #133 at 0dfeaad found a strict socket comparison passing it).
func TestAnUntimedWakeWordAFinalMovedAtTheEdgeIsNoNewCall(t *testing.T) {
	r := newLongFollowUpDrivingRig(t)
	untimed := func(int) time.Time { return time.Time{} }
	r.s.OnTranscript(timedResult("Wispr.", 10, false, untimed))
	sess := r.waitSession()
	speakAs(t, sess, "The issue is in review.")
	started := waitSpeechAfter(t, r, time.Time{})
	r.s.OnTranscript(timedResult("Wispr.", 10.3, true, untimed))
	never(t, "the call's own final, moved 0.3 s on the socket, cut the voice", 200*time.Millisecond, func() bool { return voiceCutAfter(r, started) })
}
