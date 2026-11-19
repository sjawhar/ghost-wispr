package gptlive

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// agentTexts collects the session's agent text until one equal to until
// arrives.
func agentTexts(t *testing.T, s voice.Session, until string) []string {
	t.Helper()
	var got []string
	deadline := time.After(testWait)
	for {
		select {
		case ev, ok := <-s.Events():
			if !ok {
				t.Fatalf("Events closed before agent text %q; had %q", until, got)
			}
			if ev.Kind != voice.EventAgentText {
				continue
			}
			got = append(got, ev.Text)
			if ev.Text == until {
				return got
			}
		case <-deadline:
			t.Fatalf("no agent text %q; had %q", until, got)
			return nil
		}
	}
}

// GPT-Live's transcript fragments carry no response or item id, only their
// place on the session timeline. The words of a speech Interrupt dropped are
// found by that place, however late their fragment arrives: here two seconds
// after its audio, and after the next speech began.
func TestTheWordsOfACutSpeechAreDroppedHoweverLateTheyArrive(t *testing.T) {
	out := newFakeOutput()
	srv := newLiveServer(t)
	s, c, _ := openSession(t, srv, testExchange(out, newFakeTools(voice.AnswerResult{})))

	cutAt := c.sessionMs()
	c.audioDelta(ramp(1000, 2400))
	out.nextSpeech(t).waitSamples(t, 2400)
	s.Interrupt()
	c.audioDelta(ramp(1000, 2400)) // more of the cut speech, which plays nothing

	time.Sleep(speechGap + 200*time.Millisecond)
	c.audioDelta(ramp(5000, 2400))
	c.said("Sure.")
	out.nextSpeech(t)

	time.Sleep(time.Until(time.UnixMilli(c.startedAt.Load() + cutAt + 2000)))
	c.saidAt("words nobody heard", cutAt)
	c.saidAt("of it either", cutAt+150)
	c.said("Okay.")
	for _, text := range agentTexts(t, s, "Okay.") {
		if text == "words nobody heard" || text == "of it either" {
			t.Fatalf("agent text %q of the cut speech arrived 2 s late and was reported as Wispr's", text)
		}
	}
}

// The words of the speech after a cut one are Wispr's, though their fragment
// lies within the cut speech's span: a server that sends audio faster than
// it plays makes the cut speech's span, its start plus its length, run past
// the start of the next one. A fragment goes to the latest speech it fits.
func TestTheSpeechAfterACutOneKeepsItsWords(t *testing.T) {
	out := newFakeOutput()
	srv := newLiveServer(t)
	s, c, _ := openSession(t, srv, testExchange(out, newFakeTools(voice.AnswerResult{})))

	c.audioDelta(ramp(1000, 2400))
	out.nextSpeech(t).waitSamples(t, 2400)
	s.Interrupt()
	c.audioDelta(ramp(1000, SampleRate))         // the rest of the cut speech, a second of it at once
	time.Sleep(speechGap + 200*time.Millisecond) // the second still runs on, on the cut speech's span
	next := c.sessionMs()
	c.audioDelta(ramp(5000, 2400))
	out.nextSpeech(t)
	c.saidAt("Here is the answer.", next)
	c.said("Okay.")
	got := agentTexts(t, s, "Okay.")
	if got[0] != "Here is the answer." {
		t.Fatalf("agent text %q, want the next speech's first words", got)
	}
}

// The probe measures how far GPT-Live's transcript fragments fall outside
// the speech they belong to, as the session places that speech on its own
// clock, to size the margin around a cut speech.
func TestTheSessionMeasuresTheTranscriptsTimelineOffset(t *testing.T) {
	out := newFakeOutput()
	srv := newLiveServer(t)
	s, c, _ := openSession(t, srv, testExchange(out, newFakeTools(voice.AnswerResult{})))

	at := c.sessionMs()
	c.audioDelta(ramp(1000, 2400)) // 100 ms
	c.audioDelta(ramp(1000, 2400)) // 100 ms
	c.saidAt("Inside.", at+100)
	time.Sleep(speechGap + 200*time.Millisecond)
	at = c.sessionMs()
	c.audioDelta(ramp(1000, 2400))
	c.saidAt("Early.", at-120)
	c.said("Done.")
	agentTexts(t, s, "Done.")
	m, ok := s.(interface{ TimelineOffsets() []time.Duration })
	if !ok {
		t.Fatal("the session does not report its timeline offsets")
	}
	got := m.TimelineOffsets()
	if len(got) != 3 {
		t.Fatalf("offsets %v, want one per fragment of uncut speech (3)", got)
	}
	for i, want := range []time.Duration{0, -120 * time.Millisecond, 0} {
		if d := got[i] - want; d < -40*time.Millisecond || d > 40*time.Millisecond {
			t.Errorf("offset %d is %v, want about %v: how far the fragment falls outside its speech", i, got[i], want)
		}
	}
}

// N1: a delegation that heard nothing new asks the last question again, not
// everything heard since Wispr last spoke.
func TestADelegationWithNothingNewAsksTheLastQuestion(t *testing.T) {
	x := testExchange(newFakeOutput(), newFakeTools(voice.AnswerResult{Say: "x"}))
	x.AskerTurn = ""
	srv := newLiveServer(t)
	s, c, _ := openSession(t, srv, x)

	c.heard("What is PR 81?")
	c.delegate("item_1")
	if q := answerArgs(t, waitEvent(t, s, voice.EventToolCall).Call.Args).Question; q != "What is PR 81?" {
		t.Fatalf("first question %q", q)
	}
	c.heard(" And who holds it?")
	c.delegate("item_2")
	if q := answerArgs(t, waitEvent(t, s, voice.EventToolCall).Call.Args).Question; q != "And who holds it?" {
		t.Fatalf("second question %q", q)
	}
	c.delegate("item_3")
	if q := answerArgs(t, waitEvent(t, s, voice.EventToolCall).Call.Args).Question; q != "And who holds it?" {
		t.Fatalf("question with nothing new heard %q, want the last question", q)
	}
}

// N2: the Supervisor's Recent already ends with the wake sentence, which
// holds the asker's words; the history and the answer tool's transcript hold
// them once.
func TestTheAskersWordsAreInTheHistoryOnce(t *testing.T) {
	tools := newFakeTools(voice.AnswerResult{Say: "x"})
	x := testExchange(newFakeOutput(), tools)
	x.Recent = append(x.Recent, voice.TurnText{Speaker: 2, Text: "Wispr, what is happening with ABC-123?"})
	srv := newLiveServer(t)
	s, c, start := openSession(t, srv, x)

	if n := strings.Count(words(seedText(t, start.Session.Input)), "what is happening with abc 123"); n != 1 {
		t.Errorf("the seeded history holds the asker's words %d times, want once", n)
	}
	c.delegate("item_h")
	args := answerArgs(t, waitEvent(t, s, voice.EventToolCall).Call.Args)
	if n := strings.Count(words(args.Transcript), "what is happening with abc 123"); n != 1 {
		t.Errorf("the answer tool's transcript holds the asker's words %d times, want once:\n%s", n, args.Transcript)
	}
}

// With the asker's words not in Recent, the history and the transcript still
// hold them.
func TestTheAskersWordsAreInTheHistoryWhenRecentLacksThem(t *testing.T) {
	tools := newFakeTools(voice.AnswerResult{Say: "x"})
	srv := newLiveServer(t)
	s, c, start := openSession(t, srv, testExchange(newFakeOutput(), tools))

	if n := strings.Count(words(seedText(t, start.Session.Input)), "what is happening with abc 123"); n != 1 {
		t.Errorf("the seeded history holds the asker's words %d times, want once", n)
	}
	c.delegate("item_h")
	args := answerArgs(t, waitEvent(t, s, voice.EventToolCall).Call.Args)
	if n := strings.Count(words(args.Transcript), "what is happening with abc 123"); n != 1 {
		t.Errorf("the answer tool's transcript holds the asker's words %d times, want once:\n%s", n, args.Transcript)
	}
}

// M22: a delegation that arrives while Close waits for the server runs no
// tool.
func TestADelegationDuringCloseRunsNoTool(t *testing.T) {
	tools := newFakeTools(voice.AnswerResult{Say: "x"})
	srv := newLiveServer(t)
	s, c, _ := openSession(t, srv, testExchange(newFakeOutput(), tools))
	c.heard("Wispr, what about ABC-123?")
	time.Sleep(transcriptSettle + 50*time.Millisecond)
	closed := make(chan struct{})
	go func() { _ = s.Close(); close(closed) }()
	c.expect(evSessionClose)
	c.delegate("item_late")
	<-closed
	select {
	case call := <-tools.calls:
		t.Fatalf("a delegation that arrived during Close ran the tool: %s", call.ID)
	case <-time.After(100 * time.Millisecond):
	}
}

// Na2: the question waits for its fragments to settle.
func TestTheQuestionWaitsForTheTranscriptToSettle(t *testing.T) {
	x := testExchange(newFakeOutput(), newFakeTools(voice.AnswerResult{Say: "It is in review."}))
	x.AskerTurn = ""
	srv := newLiveServer(t)
	s, c, _ := openSession(t, srv, x)
	c.delegate("item_s")
	time.Sleep(50 * time.Millisecond)
	c.heard("Wispr, what is the status of ")
	time.Sleep(100 * time.Millisecond)
	c.heard("ABC-123?")
	var args voice.AnswerArgs
	if err := json.Unmarshal(waitEvent(t, s, voice.EventToolCall).Call.Args, &args); err != nil {
		t.Fatal(err)
	}
	if args.Question != "Wispr, what is the status of ABC-123?" {
		t.Fatalf("question %q, want both fragments", args.Question)
	}
}

// Open's comment says the context bounds the session; the behaviour is
// TestTheSessionEndsWithItsExchangesContext. This pins that a context that
// ends after Open returns still ends the session.
func TestAContextThatEndsAfterOpenEndsTheSession(t *testing.T) {
	srv := newLiveServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	s, c, _ := openSessionCtx(t, srv, ctx, testExchange(newFakeOutput(), newFakeTools(voice.AnswerResult{})))
	c.expect(evSessionClose)
	waitClosed(t, s.Events())
}

// R4x3: the model's words that arrive while Close waits for session.closed
// are not Wispr's speech: nothing of them played.
func TestTheModelsWordsDuringCloseAreNotWisprs(t *testing.T) {
	srv := newLiveServer(t)
	s, c, _ := openSession(t, srv, testExchange(newFakeOutput(), newFakeTools(voice.AnswerResult{})))
	got := make(chan string, 16)
	go func() {
		defer close(got)
		for ev := range s.Events() {
			if ev.Kind == voice.EventAgentText {
				got <- ev.Text
			}
		}
	}()
	served := make(chan struct{})
	go func() {
		defer close(served)
		for ev := range c.in {
			if ev.Type == evSessionClose {
				break
			}
		}
		c.said("words after close")
		c.send(map[string]any{"type": evSessionClosed, "event_id": "event_closed", "reason": "close_requested", "usage": map[string]any{"seconds": 1}})
	}()
	_ = s.Close()
	<-served
	for text := range got {
		t.Fatalf("agent text %q arrived while the session closed", text)
	}
}

// R4x2: an Interrupt after a speech has ended cuts nothing: that speech
// played, and its words, however late they arrive, are Wispr's.
func TestAnInterruptAfterASpeechEndedCutsNothing(t *testing.T) {
	out := newFakeOutput()
	srv := newLiveServer(t)
	s, c, _ := openSession(t, srv, testExchange(out, newFakeTools(voice.AnswerResult{})))

	at := c.sessionMs()
	c.audioDelta(ramp(1000, 2400))
	out.nextSpeech(t).waitSamples(t, 2400)
	waitEvent(t, s, voice.EventSpeechEnded)
	time.Sleep(speechGap + 100*time.Millisecond)
	s.Interrupt()
	c.saidAt("It is in review.", at)
	c.said("Okay.")
	if got := agentTexts(t, s, "Okay."); got[0] != "It is in review." {
		t.Fatalf("agent text %q, want the ended speech's words: an Interrupt after it cut nothing", got)
	}
}
