package voice

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/monoclock"
)

// TestEveryTimingEventOfTheFirstExchangeNamesItsSession fails if a timing
// event of a recording session's first exchange names no session, as its wake
// and its wake sound did: the session starts when Deepgram's first
// speech_final or UtteranceEnd flushes its finals, after the wake was heard
// on an interim result.
func TestEveryTimingEventOfTheFirstExchangeNamesItsSession(t *testing.T) {
	c := newFakeConcierge("The voice issue is in review.")
	r := newRigWith(t, c, func(cfg *SupervisorConfig, _ *rig) { cfg.TurnWait = 5 * time.Second })
	r.setSession("")
	r.heard("Wispr,", false, false) // an interim result, before any session
	sess := r.waitSession()
	// The wake sound reports its first frame before it writes its second.
	eventually(t, "the wake sound's second frame", func() bool { return r.speaker.written(audio.InputChime) >= 2 })
	r.setSession("sess-1") // Deepgram's first speech_final starts the session
	r.heard("Wispr, what is happening?", true, true)
	r.waitTurn()
	waitSaid(t, sess)
	r.speaker.finish(audio.InputVoice)
	r.waitEvent("voice_spoken:sess-1")
	waitTimings(t, r, "wake_heard", "wake_chime_queued", "turn_end", "first_audio", "fact_audio", "speech_end")
	var wakeAt, turnAt time.Time
	for _, l := range r.timingLines() {
		if l.sessionID != "sess-1" {
			t.Errorf("%s names session %q, want sess-1", l.event, l.sessionID)
		}
		switch l.event {
		case "wake_heard":
			wakeAt = l.at.Wall
		case "turn_end":
			turnAt = l.at.Wall
		}
	}
	if !wakeAt.Before(turnAt) {
		t.Errorf("wake_heard says %v and turn_end %v: the wake must keep the time it was heard", wakeAt, turnAt)
	}
	waitTimings(t, r, "turn_signal")
}

// TestAHeldAnswerSpokenOnGoAheadReportsItsAudioWithItsSession fails if a
// holder's answer spoken on "Wispr, go ahead" does not report its first audio
// and its fact's audio, or reports the end of its speech with no session: the
// speech ends after the answer's exchange did, as audio.Output reports a
// speech's end speechEndHold after its last audio.
func TestAHeldAnswerSpokenOnGoAheadReportsItsAudioWithItsSession(t *testing.T) {
	c := newFakeConcierge()
	r := newRig(t, c)
	c.answers <- PendingAnswer{TaskID: "t1", SessionID: "sess-1", Text: "It ships tomorrow."}
	r.waitEvent("voice_answer_ready:sess-1")
	r.heard("Wispr, go ahead.", true, true)
	answer := r.waitSession()
	if got := waitSaid(t, answer); got != "It ships tomorrow." {
		t.Fatalf("said %q, want the held answer", got)
	}
	r.waitSpeaking()
	r.speaker.drain(audio.InputVoice)
	eventually(t, "the held answer's exchange to end", func() bool {
		r.s.mu.Lock()
		defer r.s.mu.Unlock()
		return r.s.ex == nil
	})
	r.speaker.endSpeech(audio.InputVoice)
	waitTimings(t, r, "speech_end")
	for _, l := range r.timingLines() {
		if l.sessionID != "sess-1" {
			t.Errorf("%s names session %q, want sess-1", l.event, l.sessionID)
		}
	}
	waitTimings(t, r, "first_audio", "fact_audio")
}

// TestTurnEndNamesTheSignalItActedOn fails if the end of the asker's turn is
// not reported with the signal that ended it: a turn_signal timing event of
// its own, before turn_end, naming its kind and nothing else (no words), at
// the time the signal reached the unit. The signal is Deepgram's speech_final
// on a final result, its UtteranceEnd, or the turn wait running out.
func TestTurnEndNamesTheSignalItActedOn(t *testing.T) {
	const turnWait = 300 * time.Millisecond // the rig's
	for _, tc := range []struct {
		kind string
		end  func(r *rig) time.Time // ends the turn, and returns when the signal arrived
	}{
		{"speech_final", func(r *rig) time.Time {
			tr := transcriptOf("Wispr, what is happening?", 0, true, true, 0)
			tr.At = time.Now().Add(-150 * time.Millisecond)
			r.s.OnTranscript(&tr)
			return tr.At
		}},
		{"utterance_end", func(r *rig) time.Time {
			r.heard("Wispr, what is happening?", true, false)
			now := monoclock.Now()
			at := monoclock.Stamp{Wall: now.Wall.Add(-150 * time.Millisecond), Mono: now.Mono - int64(150*time.Millisecond)}
			r.s.OnUtteranceEnd(0, true, at)
			return at.Wall
		}},
		{"turn_wait", func(r *rig) time.Time {
			r.heard("Wispr, what is happening?", true, false)
			return time.Now().Add(turnWait)
		}},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			r := newRig(t, newFakeConcierge("The voice issue is in review."))
			at := tc.end(r)
			r.waitTurn()
			waitTimings(t, r, "turn_signal", "turn_end")
			var signals []timingLine
			var turnEnd timingLine
			for _, l := range r.timingLines() {
				switch l.event {
				case "turn_signal":
					signals = append(signals, l)
				case "turn_end":
					turnEnd = l
					if len(signals) == 0 {
						t.Fatal("turn_end came before its turn_signal")
					}
				}
			}
			if len(signals) != 1 {
				t.Fatalf("%d turn_signal events, want 1: %+v", len(signals), signals)
			}
			s := signals[0]
			if !slices.Equal(s.kv, []any{"kind", tc.kind}) || s.sessionID != "sess-1" {
				t.Fatalf("turn_signal carries %v for session %q, want [kind %s] for sess-1", s.kv, s.sessionID, tc.kind)
			}
			if tc.kind == "turn_wait" {
				if s.at.Wall.Before(at.Add(-50*time.Millisecond)) || s.at.Wall.After(turnEnd.at.Wall) {
					t.Fatalf("turn_signal at %v, want when the turn wait ran out (about %v), no later than turn_end at %v", s.at.Wall, at, turnEnd.at.Wall)
				}
				return
			}
			if !s.at.Wall.Equal(at) {
				t.Fatalf("turn_signal at %v, want %v, when the %s arrived", s.at.Wall, at, tc.kind)
			}
		})
	}
}

// TestATimingEventAfterItsExchangeEndedNamesNoLaterSession fails if an
// exchange that ended with no session takes a later session for a timing
// event reported after its end, as a wake sound's first frame can be: that
// session's words are not its.
func TestATimingEventAfterItsExchangeEndedNamesNoLaterSession(t *testing.T) {
	r := newRig(t, newFakeConcierge())
	r.setSession("")
	r.heard("Wispr,", false, false)
	r.waitSession()
	r.s.mu.Lock()
	ex := r.s.ex
	r.s.endLocked(ex, true, "stopped before any session")
	r.s.mu.Unlock()
	r.setSession("later-session")
	// A first frame written before the end, reported once the lock is free.
	r.s.mu.Lock()
	now := monoclock.Now()
	r.s.exTimingLocked(ex, "wake_chime_queued", monoclock.Stamp{Wall: now.Wall.Add(-time.Second), Mono: now.Mono - int64(time.Second)})
	r.s.mu.Unlock()
	for _, l := range r.timingLines() {
		if l.sessionID != "" {
			t.Errorf("%s names session %q; its exchange ended with none", l.event, l.sessionID)
		}
	}
}

// TestTheEndOfSpeechNamesTheSessionItsSpeechWasIn fails if speech_end names
// another session than its speech's: an exchange's speech that ends before
// the exchange has its session must wait for it, and speech of no exchange
// that began with no session names none, even once a later session began.
// Speech of no exchange that began in a session keeps it.
func TestTheEndOfSpeechNamesTheSessionItsSpeechWasIn(t *testing.T) {
	t.Run("an exchange's speech before its session", func(t *testing.T) {
		c := newFakeConcierge("First fact.", "Second fact.")
		r := newRigWith(t, c, func(cfg *SupervisorConfig, _ *rig) { cfg.FollowUpWait = 5 * time.Second })
		r.setSession("")
		r.heard("Wispr, what is happening?", false, false) // answered on the turn wait
		first := r.waitSession()
		r.waitTurn()
		// Both facts are written before the speech plays out: a fact written
		// after it would start a speech that never ends here.
		waitSaid(t, first)
		waitSaid(t, first)
		r.waitSpeaking()
		r.speaker.finish(audio.InputVoice)
		eventually(t, "the follow-up window", func() bool {
			r.s.mu.Lock()
			defer r.s.mu.Unlock()
			return r.s.ex != nil && r.s.ex.followUp
		})
		r.setSession("sess-1")
		r.heard("What happens next?", true, true)
		r.waitTurn()
		waitTimings(t, r, "speech_end")
		for _, l := range r.timingLines() {
			if l.event == "speech_end" && l.sessionID != "sess-1" {
				t.Errorf("speech_end names session %q, want its exchange's sess-1", l.sessionID)
			}
		}
	})
	for _, tc := range []struct{ name, startSession, want string }{
		{"speech of no exchange with no session", "", ""},
		{"speech of no exchange in a session", "sess-1", "sess-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, newFakeConcierge())
			r.setSession(tc.startSession)
			r.s.onSpeech(&audio.SpeechEvent{Input: audio.InputVoice, Started: true, At: time.Now()})
			r.setSession("later-session")
			r.s.onSpeech(&audio.SpeechEvent{Input: audio.InputVoice, At: time.Now()})
			waitTimings(t, r, "speech_end")
			for _, l := range r.timingLines() {
				if l.event == "speech_end" && l.sessionID != tc.want {
					t.Errorf("speech_end names session %q, want %q", l.sessionID, tc.want)
				}
			}
		})
	}
}

// TestTheTurnWaitIsStampedWhenItRanOut fails if the turn wait's turn_signal
// is stamped once the Supervisor's lock is free rather than when the wait ran
// out: the time it waited for the lock is the unit's own, and belongs between
// turn_signal and turn_end.
func TestTheTurnWaitIsStampedWhenItRanOut(t *testing.T) {
	r := newRigWith(t, newFakeConcierge("The voice issue is in review."), func(cfg *SupervisorConfig, _ *rig) { cfg.TurnWait = time.Hour })
	r.heard("Wispr, what is happening?", true, false)
	r.waitSession()
	r.s.mu.Lock()
	ex := r.s.ex
	ranOut := time.Now()
	done := make(chan struct{})
	go func() { r.s.turnWaited(ex); close(done) }()
	time.Sleep(200 * time.Millisecond) // the lock is held meanwhile
	released := time.Now()
	r.s.mu.Unlock()
	<-done
	waitTimings(t, r, "turn_signal", "turn_end")
	if at := r.timingOf("turn_signal"); at.Before(ranOut) || !at.Before(released) {
		t.Fatalf("turn_signal at %v, want when the wait ran out, %v or just after, before the lock came free at %v", at, ranOut, released)
	}
}

// TestHeldTimingLinesAreWrittenAsTheSessionStarts fails if the timing lines
// an exchange held for its session wait past the session's start for the
// next transcript, speech event or the exchange's end: the acceptance driver
// acts on first_audio as it comes. Here the turn ends on the turn wait before
// any session, and the session starts while the voice speaks.
func TestHeldTimingLinesAreWrittenAsTheSessionStarts(t *testing.T) {
	r := newRig(t, newFakeConcierge("The voice issue is in review."))
	r.setSession("")
	r.heard("Wispr, what is happening?", true, false) // answered on the turn wait
	sess := r.waitSession()
	r.waitTurn()
	waitSaid(t, sess)
	r.waitSpeaking()
	eventually(t, "the exchange to hold its first audio", func() bool {
		r.s.mu.Lock()
		defer r.s.mu.Unlock()
		return r.s.ex != nil && r.s.ex.firstOutDone
	})
	if lines := r.timingLines(); len(lines) != 0 {
		t.Fatalf("control: before any session the exchange wrote %+v, want its lines held", lines)
	}
	r.startSession("sess-1")
	var events []string
	for _, l := range r.timingLines() {
		events = append(events, l.event)
		if l.sessionID != "sess-1" {
			t.Errorf("%s names session %q, want sess-1", l.event, l.sessionID)
		}
	}
	if !slices.Contains(events, "first_audio") {
		t.Errorf("as the session started the exchange wrote %v, want its held first_audio among them", events)
	}
	r.speaker.finish(audio.InputVoice)
}

// TestTheExchangeTakesTheSessionThatStartedAfterItOpened fails if an exchange
// that opened with no session is bound to any session but the start after it
// opened, on any binding path: the session-started hook, a timing or speech
// event, a transcript, or its end. Here that start, A, settles and ends, and a
// later one, B, becomes current, before anything binds the exchange: the
// binding must look A up by its sequence, not take B because it is current.
// An exchange that ends before any start settles names no session, and the
// log says its session is unknown; never a later one.
func TestTheExchangeTakesTheSessionThatStartedAfterItOpened(t *testing.T) {
	// open opens an exchange with no session, after the rig's first session
	// (sequence 1) ended, and waits for its held lines: it waits for start 2.
	open := func(t *testing.T) (*rig, func() []string) {
		var logs []string
		var mu sync.Mutex
		r := newRigWith(t, newFakeConcierge(), func(cfg *SupervisorConfig, _ *rig) {
			cfg.TurnWait = time.Minute
			cfg.Logf = func(format string, args ...any) {
				mu.Lock()
				defer mu.Unlock()
				logs = append(logs, fmt.Sprintf(format, args...))
			}
		})
		r.setSession("")
		r.heard("Wispr,", false, false)
		r.waitSession()
		// The wake sound reports its first frame from its own goroutine: wait
		// until that line is held too, so no line comes later and binds the
		// exchange before the test's hooks do.
		eventually(t, "the wake's held lines, its sound's first frame included", func() bool {
			r.s.mu.Lock()
			defer r.s.mu.Unlock()
			return r.s.ex != nil && slices.ContainsFunc(r.s.ex.unsent, func(e timingEvent) bool { return e.event == "wake_chime_queued" })
		})
		return r, func() []string {
			mu.Lock()
			defer mu.Unlock()
			return slices.Clone(logs)
		}
	}
	// aThenB settles A (start 2), ends it, and settles B (start 3), now
	// current, all without running a hook.
	aThenB := func(r *rig) {
		r.setSession("sess-a")
		r.setSession("")
		r.setSession("sess-b")
	}
	all := func(t *testing.T, r *rig, want string) {
		t.Helper()
		lines := r.timingLines()
		if len(lines) == 0 {
			t.Fatal("control: no timing line was written")
		}
		for _, l := range lines {
			if l.sessionID != want {
				t.Errorf("%s names session %q, want %q", l.event, l.sessionID, want)
			}
		}
	}
	end := func(r *rig) {
		r.s.mu.Lock()
		defer r.s.mu.Unlock()
		r.s.endLocked(r.s.ex, true, "the test ends it")
	}

	t.Run("B's hook comes first, then A's", func(t *testing.T) {
		r, _ := open(t)
		aThenB(r)
		r.s.SessionStarted("sess-b", 3)
		if lines := r.timingLines(); len(lines) != 0 {
			t.Fatalf("B's hook wrote %+v; the exchange waits for A", lines)
		}
		r.s.SessionStarted("sess-a", 2)
		all(t, r, "sess-a")
	})
	t.Run("a speech event before A's hook", func(t *testing.T) {
		r, _ := open(t)
		aThenB(r)
		r.s.onSpeech(&audio.SpeechEvent{Input: audio.InputVoice, Started: true, At: time.Now()})
		r.s.SessionStarted("sess-a", 2) // late: the exchange is bound already
		all(t, r, "sess-a")
	})
	t.Run("a transcript before A's hook", func(t *testing.T) {
		r, _ := open(t)
		aThenB(r)
		r.heard("Wispr, what is happening?", true, false)
		all(t, r, "sess-a")
	})
	t.Run("A's hook never comes", func(t *testing.T) {
		r, _ := open(t)
		aThenB(r)
		end(r)
		all(t, r, "sess-a")
	})
	t.Run("no start settles before the end", func(t *testing.T) {
		r, logged := open(t)
		end(r)
		r.setSession("sess-b") // after the end: not the exchange's
		all(t, r, "")
		if logs := logged(); !slices.ContainsFunc(logs, func(l string) bool { return strings.Contains(l, "its session is unknown") }) {
			t.Errorf("the log holds %q, want a line that the exchange's session is unknown", logs)
		}
	})
}
