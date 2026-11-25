package voice

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

// questionLog is the question log a rig's Supervisor writes to
// (SupervisorConfig.Question).
type questionLog struct {
	mu  sync.Mutex
	got []Question
	ch  chan Question
}

func (l *questionLog) add(q *Question) {
	l.mu.Lock()
	l.got = append(l.got, *q)
	l.mu.Unlock()
	l.ch <- *q
}

// next waits for the next question the Supervisor logs.
func (l *questionLog) next(t *testing.T) Question {
	t.Helper()
	select {
	case q := <-l.ch:
		return q
	case <-time.After(testWait):
		t.Fatal("no question was logged")
		return Question{}
	}
}

// none fails if a question is logged within d.
func (l *questionLog) none(t *testing.T, what string, d time.Duration) {
	t.Helper()
	select {
	case q := <-l.ch:
		t.Fatalf("%s: logged %+v", what, q)
	case <-time.After(d):
	}
}

// newQuestionRig is newRig over c, or newDrivingRig's voice when driving,
// with its questions logged.
func newQuestionRig(t *testing.T, c *fakeConcierge, driving bool) (*rig, *questionLog) {
	t.Helper()
	log := &questionLog{ch: make(chan Question, 16)}
	r := newRigWith(t, c, func(cfg *SupervisorConfig, r *rig) {
		cfg.Question = log.add
		if driving {
			v := drivingVoice{r.voice}
			cfg.Voice = func() (Voice, error) { return v, nil }
			cfg.IdleWait = time.Minute
		}
	})
	return r, log
}

// TestATurnTheConciergeAnsweredIsLogged fails if a turn voice A answered is
// not logged once, after its answer played to its end, with the turn as
// heard after the wake phrase, the voice, the issue the Concierge took, its
// outcome, the lines spoken, and the times of its words, of its first audio
// and of its end.
func TestATurnTheConciergeAnsweredIsLogged(t *testing.T) {
	c := newFakeConcierge("ABC-12 is in review.")
	c.acks = []string{"Reading ABC-12."}
	c.outcome, c.issueKey = OutcomeAnswered, "ABC-12"
	r, log := newQuestionRig(t, c, false)
	before := time.Now()
	sess := r.ask("what is happening with ABC-12?")
	waitSaid(t, sess)
	waitSaid(t, sess) // both lines written: the speaker has them to play
	log.none(t, "the question was logged while its answer still played", 150*time.Millisecond)
	r.speaker.finish(audio.InputVoice)
	q := log.next(t)
	if q.SessionID != "sess-1" || q.Heard != "what is happening with ABC-12?" || q.VendorWording != "" || q.Engine != "fake" ||
		q.IssueKey != "ABC-12" || q.Outcome != OutcomeAnswered || q.Answer != "Reading ABC-12. ABC-12 is in review." {
		t.Errorf("logged %+v", q)
	}
	// The rig's words carry no capture time, so each is taken as said when
	// its result came (TestATurnsWordsAreTimedByTheirCapture times them).
	if q.AskedAt.Before(before) || q.HeardFrom.Before(before) || q.HeardFrom.After(q.AskedAt) || !q.HeardTo.After(q.HeardFrom) {
		t.Errorf("times: asked %v, heard %v to %v; want the words heard from the call on, after %v", q.AskedAt, q.HeardFrom, q.HeardTo, before)
	}
	if q.FirstAudio == nil || *q.FirstAudio < 0 || q.Took < *q.FirstAudio {
		t.Errorf("first audio %v, took %v; want a first audio before the end", q.FirstAudio, q.Took)
	}
	log.none(t, "a second row for one turn", 300*time.Millisecond)
}

// TestATurnsWordsAreTimedByTheirCapture fails if a turn's heard span, which
// places its question among the segments a deletion erases, is not from its
// first word's capture time to its last word's end across the results that
// hold the turn, or if a new turn keeps the span of the one before.
func TestATurnsWordsAreTimedByTheirCapture(t *testing.T) {
	captured := time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)
	result := func(at time.Duration, words ...string) *Transcript {
		tr := &Transcript{At: captured.Add(at + 3*time.Second)} // the result came 3 s after
		for i, w := range words {
			start := 0.5 * float64(i)
			tr.Words = append(tr.Words, Word{Text: w, Start: start, End: start + 0.25, At: captured.Add(at + time.Duration(start*float64(time.Second)))})
		}
		return tr
	}
	ex := &exchange{}
	ex.questionHeardLocked(result(0, "Wispr,", "what", "about"))
	ex.questionHeardLocked(result(2*time.Second, "ABC-12?"))
	if !ex.wordsFrom.Equal(captured) || !ex.wordsTo.Equal(captured.Add(2250*time.Millisecond)) {
		t.Errorf("the turn was heard %v to %v, want %v to 2.25 s on", ex.wordsFrom, ex.wordsTo, captured)
	}
	ex.x.Turn++
	ex.questionHeardLocked(result(10*time.Second, "and", "who"))
	if !ex.wordsFrom.Equal(captured.Add(10*time.Second)) || !ex.wordsTo.Equal(captured.Add(10750*time.Millisecond)) {
		t.Errorf("the next turn was heard %v to %v, want 10 s to 10.75 s on", ex.wordsFrom, ex.wordsTo)
	}
}

// TestEachOutcomeIsLogged fails if a turn voice A handled is logged with an
// outcome other than how it ended: the Concierge's own (answered, asked back,
// picked, asked an agent, a "no" to its offer), Answer's error (an outage, a
// failure), a voice that cannot speak, or a stop, a barge-in and "Wispr,
// never mind" that cut the answer.
func TestEachOutcomeIsLogged(t *testing.T) {
	for _, tc := range []struct {
		name    string
		setup   func(c *fakeConcierge, r *rig)
		cut     string // said over the answer, while the Concierge holds it
		want    QuestionOutcome
		answer  string
		spoken  bool // the answer plays to its end
		noAudio bool
	}{
		{name: "answered", setup: func(c *fakeConcierge, r *rig) { c.outcome = OutcomeAnswered }, want: OutcomeAnswered, answer: "PR 80 is in review.", spoken: true},
		{name: "asked back", setup: func(c *fakeConcierge, r *rig) { c.outcome = OutcomeAskedBack }, want: OutcomeAskedBack, answer: "PR 80 is in review.", spoken: true},
		{name: "picked", setup: func(c *fakeConcierge, r *rig) { c.outcome = OutcomePicked }, want: OutcomePicked, answer: "PR 80 is in review.", spoken: true},
		{name: "asked an agent", setup: func(c *fakeConcierge, r *rig) { c.outcome = OutcomeAskedAgent }, want: OutcomeAskedAgent, answer: "PR 80 is in review.", spoken: true},
		{name: "a no to the offer", setup: func(c *fakeConcierge, r *rig) { c.outcome = OutcomeNeverMind }, want: OutcomeNeverMind, answer: "PR 80 is in review.", spoken: true},
		{name: "outage", setup: func(c *fakeConcierge, r *rig) {
			c.outcome, c.answerErr = OutcomeAnswered, fmt.Errorf("concierge: %w", ErrUnreachable)
		}, want: OutcomeOutage, answer: "PR 80 is in review.", spoken: true},
		{name: "the model failed", setup: func(c *fakeConcierge, r *rig) {
			c.lines, c.answerErr = nil, errors.New("the model failed")
		}, want: OutcomeFailed, answer: sorryLine, spoken: true},
		{name: "the voice cannot speak", setup: func(c *fakeConcierge, r *rig) {
			c.outcome = OutcomeAnswered
			r.voice.failSays(errors.New("the vendor dropped the connection"))
		}, want: OutcomeFailed, noAudio: true},
		{name: "Wispr, stop", setup: func(c *fakeConcierge, r *rig) { c.outcome, c.hold = OutcomeAnswered, true }, cut: "Wispr, stop.", want: OutcomeStopped, answer: "PR 80 is in review."},
		{name: "barge-in", setup: func(c *fakeConcierge, r *rig) { c.outcome, c.hold = OutcomeAnswered, true }, cut: "no no that is the wrong one", want: OutcomeStopped, answer: "PR 80 is in review."},
		{name: "Wispr, never mind", setup: func(c *fakeConcierge, r *rig) { c.outcome, c.hold = OutcomeAskedAgent, true }, cut: "Wispr, never mind.", want: OutcomeNeverMind, answer: "PR 80 is in review."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newFakeConcierge("PR 80 is in review.")
			r, log := newQuestionRig(t, c, false)
			tc.setup(c, r)
			r.ask("what is happening with ABC-12?")
			switch {
			case tc.cut != "":
				r.waitSpeaking()
				r.heard(tc.cut, true, true)
			case tc.spoken:
				r.waitSpeaking()
				r.speaker.finish(audio.InputVoice)
			}
			q := log.next(t)
			if q.Outcome != tc.want || q.Answer != tc.answer || q.Heard != "what is happening with ABC-12?" {
				t.Errorf("logged %+v; want outcome %q, answer %q", q, tc.want, tc.answer)
			}
			if (q.FirstAudio == nil) != tc.noAudio {
				t.Errorf("first audio %v, want it only when the voice spoke", q.FirstAudio)
			}
		})
	}
}

// TestSideTalkAfterWisprsQuestionIsLogged fails if a reply to Wispr's own
// question that the Concierge took for side talk is not logged as side talk,
// with nothing said, after the turn that asked it.
func TestSideTalkAfterWisprsQuestionIsLogged(t *testing.T) {
	c := newFakeConcierge("Did you mean A B C twelve, or A B C thirteen?")
	c.outcome = OutcomeAskedBack
	c.turnErrs = map[string]error{"okay, let's move on to the budget": fmt.Errorf("concierge: %w", ErrSideTalk)}
	r, log := newQuestionRig(t, c, false)
	r.ask("what is happening with the realtime voice?")
	r.waitSpeaking()
	r.speaker.finish(audio.InputVoice)
	if q := log.next(t); q.Outcome != OutcomeAskedBack {
		t.Fatalf("the first turn logged %+v, want asked back", q)
	}
	r.heard("okay, let's move on to the budget", true, true)
	r.waitTurn()
	q := log.next(t)
	if q.Outcome != OutcomeSideTalk || q.Heard != "okay, let's move on to the budget" || q.Answer != "" || q.FirstAudio != nil {
		t.Errorf("the reply logged %+v, want side talk, heard as said, with nothing said", q)
	}
}

// TestEachTurnOfAnExchangeIsLoggedAndNothingElse fails if the two turns of
// one exchange, a question and a follow-up question, are not logged one row
// each, or if what never reached the Concierge is logged: a wake with no
// question, and side talk in the follow-up window.
func TestEachTurnOfAnExchangeIsLoggedAndNothingElse(t *testing.T) {
	c := newFakeConcierge("PR 80 is in review.")
	c.outcome = OutcomeAnswered
	r, log := newQuestionRig(t, c, false)
	sess := r.ask("what is happening with ABC-12?")
	waitSaid(t, sess)
	r.speaker.finish(audio.InputVoice)
	if q := log.next(t); q.Heard != "what is happening with ABC-12?" {
		t.Fatalf("the first turn logged %+v", q)
	}
	r.heard("And who holds it?", true, true)
	r.waitTurn()
	waitSaid(t, sess) // its frame is written: the speaker has it to play
	r.speaker.finish(audio.InputVoice)
	if q := log.next(t); q.Heard != "And who holds it?" || q.Outcome != OutcomeAnswered {
		t.Fatalf("the follow-up logged %+v", q)
	}
	r.heard("okay, thanks everyone, let's move on", true, true)
	log.none(t, "side talk in the follow-up window", 400*time.Millisecond)
	r.heard("Wispr.", true, true)
	r.waitSession()
	log.none(t, "a wake with no question", 600*time.Millisecond)
}

// TestAFollowUpAnsweredInTheSameSpeechHasItsFirstAudio fails if a follow-up
// whose answer begins to play before the speaker ended the speech of the
// answer before it, as the speaker holds a speech a moment past its last
// audio, is logged with no first audio: the proof run of the question log
// logged "And who is working on it?" so.
func TestAFollowUpAnsweredInTheSameSpeechHasItsFirstAudio(t *testing.T) {
	c := newFakeConcierge("PR 80 is in review.")
	c.outcome = OutcomeAnswered
	r, log := newQuestionRig(t, c, false)
	sess := r.ask("what is happening with ABC-12?")
	waitSaid(t, sess)
	r.speaker.drain(audio.InputVoice) // played; the speech has not ended
	if q := log.next(t); q.FirstAudio == nil {
		t.Fatalf("the first turn logged %+v, want its first audio", q)
	}
	r.heard("And who holds it?", true, true)
	r.waitTurn()
	waitSaid(t, sess) // its frame is written, in the speech still playing
	r.speaker.finish(audio.InputVoice)
	q := log.next(t)
	if q.Heard != "And who holds it?" || q.FirstAudio == nil || *q.FirstAudio < 0 || q.Took < *q.FirstAudio {
		t.Errorf("the follow-up logged %+v, want its first audio within the speech already playing", q)
	}
}

// TestASpeechStartReportedLateIsNoFirstAudio fails if the report of a speech
// that began before a turn was asked, which the speaker delivers once the
// turn's question is open, is taken for that turn's first audio: the proof
// run of the question log logged a follow-up's first audio as 0 ms so.
func TestASpeechStartReportedLateIsNoFirstAudio(t *testing.T) {
	c := newFakeConcierge("PR 80 is in review.")
	c.outcome = OutcomeAnswered
	r, log := newQuestionRig(t, c, false)
	sess := r.ask("what is happening with ABC-12?")
	waitSaid(t, sess)
	r.speaker.finish(audio.InputVoice)
	log.next(t)
	gate := make(chan struct{})
	c.mu.Lock()
	c.afterAcks = gate // the follow-up's answer waits, its question open
	c.mu.Unlock()
	r.heard("And who holds it?", true, true)
	r.waitTurn()
	r.s.mu.Lock()
	ex := r.s.ex
	r.s.mu.Unlock()
	past := time.Now().Add(-2 * time.Second)
	r.speaker.emit(&audio.SpeechEvent{Input: audio.InputVoice, Started: true, At: past, Owner: ex})
	r.speaker.FlushSpeech()
	r.speaker.emit(&audio.SpeechEvent{Input: audio.InputVoice, Started: false, At: past.Add(time.Second), Owner: ex})
	r.speaker.FlushSpeech()
	close(gate)
	waitSaid(t, sess) // its frame is written: the speaker has it to play
	r.speaker.finish(audio.InputVoice)
	q := log.next(t)
	if q.Heard != "And who holds it?" || q.FirstAudio == nil || *q.FirstAudio <= 0 {
		t.Errorf("the follow-up logged %+v, want the first audio of its own answer, after it was asked", q)
	}
}

// TestALogThatHangsChangesNoAnswer fails if a question log that never
// returns, as a write to a locked database, holds up an answer: the next
// turn is answered and spoken as with no log.
func TestALogThatHangsChangesNoAnswer(t *testing.T) {
	c := newFakeConcierge("PR 80 is in review.")
	c.outcome = OutcomeAnswered
	hung, release := make(chan struct{}, 4), make(chan struct{})
	r := newRigWith(t, c, func(cfg *SupervisorConfig, r *rig) {
		cfg.Question = func(*Question) {
			hung <- struct{}{}
			<-release
		}
	})
	t.Cleanup(func() { close(release) })
	sess := r.ask("what is happening with ABC-12?")
	r.waitSpeaking()
	r.speaker.finish(audio.InputVoice)
	select {
	case <-hung:
	case <-time.After(testWait):
		t.Fatal("the log was not called")
	}
	r.heard("And who holds it?", true, true)
	r.waitTurn()
	eventually(t, "the follow-up answered while the log hangs", func() bool {
		return slices.Equal(sess.saysCopy(), []string{"PR 80 is in review.", "PR 80 is in review."})
	})
}

// TestADrivingVoicesTurnIsLoggedWithItsVendorsWording fails if a turn voice
// B or C hands the Concierge is not logged once the voice has spoken its
// answer, with the turn as Deepgram heard it, the vendor's own wording, and
// the answer the voice was given.
func TestADrivingVoicesTurnIsLoggedWithItsVendorsWording(t *testing.T) {
	c := newFakeConcierge("ABC-12 is in review.")
	c.outcome, c.issueKey = OutcomeAnswered, "ABC-12"
	r, log := newQuestionRig(t, c, true)
	r.heard("Wispr, what is happening with ABC-12?", true, true)
	sess := r.waitSession()
	res := runAnswerTool(t, sess, "What's the status of ABC-12?")
	log.none(t, "the question was logged before the voice spoke its answer", 150*time.Millisecond)
	speakAs(t, sess, res.Say)
	r.waitSpeaking()
	r.speaker.finish(audio.InputVoice)
	q := log.next(t)
	if q.Heard != "what is happening with ABC-12?" || q.VendorWording != "What's the status of ABC-12?" || q.Engine != "fake" ||
		q.IssueKey != "ABC-12" || q.Outcome != OutcomeAnswered || q.Answer != "ABC-12 is in review." || q.FirstAudio == nil {
		t.Errorf("logged %+v", q)
	}
}

// TestABargeInOverADrivingVoicesAnswerIsLoggedStopped fails if a person who
// talks over the answer a driving voice speaks leaves its turn logged as
// answered.
func TestABargeInOverADrivingVoicesAnswerIsLoggedStopped(t *testing.T) {
	c := newFakeConcierge("ABC-12 is in review since this morning.")
	c.outcome = OutcomeAnswered
	r, log := newQuestionRig(t, c, true)
	r.heard("Wispr, what is happening with ABC-12?", true, true)
	sess := r.waitSession()
	res := runAnswerTool(t, sess, "what is happening with ABC-12?")
	speakAs(t, sess, res.Say)
	r.waitSpeaking()
	r.heard("wait, who holds it?", false, false)
	q := log.next(t)
	if q.Outcome != OutcomeStopped || q.VendorWording != "" {
		t.Errorf("logged %+v, want stopped, and no vendor wording when the vendor asked the turn as heard", q)
	}
}

// runAnswerTool runs the answer tool for question, as a vendor that drives
// its turn does, and returns its result.
func runAnswerTool(t *testing.T, sess *fakeSession, question string) AnswerResult {
	t.Helper()
	args, err := json.Marshal(AnswerArgs{Question: question})
	if err != nil {
		t.Fatal(err)
	}
	out, err := sess.x.Tools.Run(t.Context(), ToolCall{ID: "call-1", Name: AnswerTool, Args: args})
	if err != nil {
		t.Fatalf("the answer tool: %v", err)
	}
	var res AnswerResult
	if err := json.Unmarshal(out, &res); err != nil || strings.TrimSpace(res.Say) == "" {
		t.Fatalf("the answer tool returned %s (%v), want words to say", out, err)
	}
	return res
}
