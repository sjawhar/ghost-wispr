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

// The words of the speech after a cut one are Wispr's, though the cut
// speech's audio ran on in a burst before the pause.
func TestTheSpeechAfterACutOneKeepsItsWords(t *testing.T) {
	out := newFakeOutput()
	srv := newLiveServer(t)
	s, c, _ := openSession(t, srv, testExchange(out, newFakeTools(voice.AnswerResult{})))

	c.audioDelta(ramp(1000, 2400))
	out.nextSpeech(t).waitSamples(t, 2400)
	s.Interrupt()
	c.audioDelta(ramp(1000, SampleRate))         // the rest of the cut speech, a second of it at once
	time.Sleep(speechGap + 200*time.Millisecond) // the second still runs on, on the cut speech's span
	next := c.inputMs()
	c.audioDelta(ramp(5000, 2400))
	out.nextSpeech(t)
	c.saidAt("Here is the answer.", next)
	c.said("Okay.")
	got := agentTexts(t, s, "Okay.")
	if got[0] != "Here is the answer." {
		t.Fatalf("agent text %q, want the next speech's first words", got)
	}
}

// N1: a delegation re-asks the room's current asker; GPT-Live's own
// transcript heard between delegations is never the question (Main, after
// Sol's eighteenth review of #191).
func TestADelegationWithNothingNewAsksTheRoomsAsker(t *testing.T) {
	x := testExchange(newFakeOutput(), newFakeTools(voice.AnswerResult{Say: "x"}))
	rs := withRoom(x, "What is PR 81?")
	srv := newLiveServer(t)
	s, c, _ := openSession(t, srv, x)

	c.heard("What is PR 81?")
	c.delegate("item_1")
	if q := answerArgs(t, waitEvent(t, s, voice.EventToolCall).Call.Args).Question; q != "What is PR 81?" {
		t.Fatalf("first question %q", q)
	}
	rs.set(voice.RoomView{Live: true, Asker: "And who holds it?", Request: "And who holds it?", Revision: 2})
	c.heard(" And who holds it?")
	c.delegate("item_2")
	if q := answerArgs(t, waitEvent(t, s, voice.EventToolCall).Call.Args).Question; q != "And who holds it?" {
		t.Fatalf("second question %q", q)
	}
	c.delegate("item_3")
	if q := answerArgs(t, waitEvent(t, s, voice.EventToolCall).Call.Args).Question; q != "And who holds it?" {
		t.Fatalf("question with nothing new %q, want the room's asker again", q)
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
	time.Sleep(300 * time.Millisecond)
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

// Na2: the question is the room's asker, independent of GPT-Live's transcript
// fragments (Main, after Sol's eighteenth review of #191).
func TestTheQuestionIsTheRoomsAsker(t *testing.T) {
	x := testExchange(newFakeOutput(), newFakeTools(voice.AnswerResult{Say: "It is in review."}))
	withRoom(x, "Wispr, what is the status of ABC-123?")
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
		t.Fatalf("question %q, want the room's asker", args.Question)
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

// R4x2: an Interrupt after a speech has ended cuts nothing: words that
// arrive after it are Wispr's.
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
	c.speak("Okay.")
	if got := agentTexts(t, s, "Okay."); got[0] != "It is in review." {
		t.Fatalf("agent text %q, want the ended speech's words: an Interrupt after it cut nothing", got)
	}
}
