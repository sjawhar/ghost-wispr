package concierge

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/dispatch"
	"github.com/sjawhar/ghost-wispr/internal/monoclock"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// Sol's second review of #191 at 308ad445
// (https://github.com/sjawhar/ghost-wispr/pull/191#pullrequestreview-6051999112),
// from Sol's probe: a barge-in renumbered the question it began.

// cutSpeaker is r7Speaker that reports a voice speech's start and its cut,
// as the speaker output does, queued until FlushSpeech delivers them.
type cutSpeaker struct {
	r7Speaker
	eventMu sync.Mutex
	fn      func(audio.SpeechEvent)
	queue   []audio.SpeechEvent
	active  *cutPlayback
}

type cutPlayback struct {
	voice.Playback
	s       *cutSpeaker
	input   string
	owner   any
	started bool
}

func (s *cutSpeaker) Begin(input string, owner any) voice.Playback {
	return &cutPlayback{Playback: s.r7Speaker.Begin(input, owner), s: s, input: input, owner: owner}
}

func (p *cutPlayback) Write(ctx context.Context, frame []byte, format audio.AudioFormat) error {
	if err := p.Playback.Write(ctx, frame, format); err != nil {
		return err
	}
	if p.input == audio.InputVoice {
		p.s.eventMu.Lock()
		defer p.s.eventMu.Unlock()
		if !p.started {
			p.started, p.s.active = true, p
			p.s.queue = append(p.s.queue, audio.SpeechEvent{Input: p.input, Owner: p.owner, Started: true, At: time.Now()})
		}
	}
	return nil
}

func (s *cutSpeaker) Cancel(input string) {
	s.r7Speaker.Cancel(input)
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	if input == audio.InputVoice && s.active != nil {
		s.queue = append(s.queue, audio.SpeechEvent{Input: input, Owner: s.active.owner, Cancelled: true, At: time.Now()})
		s.active = nil
	}
}

func (s *cutSpeaker) OnSpeech(fn func(audio.SpeechEvent)) func() {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	s.fn = fn
	return func() {}
}

func (s *cutSpeaker) FlushSpeech() {
	s.eventMu.Lock()
	queue, fn := s.queue, s.fn
	s.queue = nil
	s.eventMu.Unlock()
	for _, e := range queue {
		fn(e)
	}
}

// cutDriver opens r7Voice sessions as a voice that drives its own turn, and
// hands each to the test.
type cutDriver struct {
	r7Voice
	opened chan *cutSession
}

type cutSession struct {
	*r7Session
	x voice.Exchange
}

func (*cutDriver) DrivesTurn() bool { return true }

func (d *cutDriver) Open(ctx context.Context, x *voice.Exchange) (voice.Session, error) {
	base, err := d.r7Voice.Open(ctx, x)
	if err != nil {
		return nil, err
	}
	s := &cutSession{r7Session: base.(*r7Session), x: *x}
	d.opened <- s
	return s, nil
}

// turnsSeen is a Concierge that keeps the exchange of each Heard and Answer.
type turnsSeen struct {
	*Concierge
	mu              sync.Mutex
	heard, answered []voice.Exchange
}

func (c *turnsSeen) Heard(ctx context.Context, x *voice.Exchange, words string) {
	c.mu.Lock()
	c.heard = append(c.heard, *x)
	c.mu.Unlock()
	c.Concierge.Heard(ctx, x, words)
}

func (c *turnsSeen) Answer(ctx context.Context, x *voice.Exchange, words string, say voice.SayFunc) error {
	c.mu.Lock()
	c.answered = append(c.answered, *x)
	c.mu.Unlock()
	return c.Concierge.Answer(ctx, x, words, say)
}

// turns are the turn numbers of the Heard calls whose words are a start of
// words, and of every answer.
func (c *turnsSeen) turns(words string) (heard, answered []uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.heard {
		if said := c.heard[i].AskerTurn; said != "" && strings.HasPrefix(words, strings.TrimRight(said, "?")) {
			heard = append(heard, c.heard[i].Turn)
		}
	}
	for i := range c.answered {
		answered = append(answered, c.answered[i].Turn)
	}
	return heard, answered
}

// cutRig is a real Supervisor over a real Concierge, with a voice that
// drives its own turn and a speaker that reports its speech and its cuts, as
// Sol's probes of #191 drive them: ABC-12, "Realtime voice budget", held by
// the live session agent-7, is every search's one clear match.
type cutRig struct {
	*rig
	seen    *turnsSeen
	speaker *cutSpeaker
	sup     *voice.Supervisor
	sess    *cutSession
	driver  *cutDriver
	// wakeEnd is when the exchange's opening question's last word ended,
	// by capture: the position of its UtteranceEnd.
	wakeEnd time.Time
}

// newCutRig opens an exchange with "Wispr, what is happening with ABC-12?";
// the model answers with turns.
func newCutRig(t *testing.T, turns ...[]*genai.GenerateContentResponse) *cutRig {
	t.Helper()
	return newCutRigOpening(t, []string{"Wispr, what is happening with ABC-12?"}, true, turns...)
}

// newCutRigOpening is newCutRig opening with finals, each a final result;
// each ends its utterance when endpoints, else the last alone.
func newCutRigOpening(t *testing.T, finals []string, endpoints bool, turns ...[]*genai.GenerateContentResponse) *cutRig {
	t.Helper()
	r := &cutRig{rig: newRig(t), speaker: &cutSpeaker{}}
	t.Cleanup(r.concierge.Stop)
	heldIssue(r.rig, "session", false)
	r.records.mu.Lock()
	r.records.hits = []dispatch.SearchHit{{Kind: "issue", IssueKey: "ABC-12", Title: "Realtime voice budget", Score: 0.9}}
	r.records.kwHits = slices.Clone(r.records.hits)
	r.records.searchDelay, r.records.kwDelay = 5*time.Millisecond, 5*time.Millisecond
	r.records.mu.Unlock()
	r.model.turns = turns
	r.seen = &turnsSeen{Concierge: r.concierge}
	driver := &cutDriver{opened: make(chan *cutSession, 1)}
	r.driver = driver
	r.sup = voice.NewSupervisor(&voice.SupervisorConfig{
		Voice:        func() (voice.Voice, error) { return driver, nil },
		Concierge:    r.seen,
		Speaker:      r.speaker,
		WakePhrases:  []string{"Wispr"},
		ProjectKeys:  func() []string { return []string{"ABC", "XYZ", "DEF", "INFRA"} },
		Session:      func() string { return "meeting-1" },
		Red:          func() bool { return true },
		IdleWait:     time.Minute,
		FollowUpWait: time.Minute,
		Logf:         t.Logf,
	})
	t.Cleanup(r.sup.Close)
	for i, final := range finals {
		r.wakeEnd = r.hearEnding(final, true, endpoints || i == len(finals)-1, nil)
	}
	select {
	case r.sess = <-driver.opened:
	case <-time.After(r7Deadline):
		t.Fatal("no voice opened")
	}
	return r
}

// hear delivers a result of text, its words captured at at, or 0.2 s apart
// up to now when at is nil; a final one ends the utterance (speech_final).
// It returns when its last word ended, by capture.
func (r *cutRig) hear(text string, final bool, at []time.Time) time.Time {
	return r.hearEnding(text, final, final, at)
}

// hearEnding is hear, the result ending its utterance when speechFinal.
func (r *cutRig) hearEnding(text string, final, speechFinal bool, at []time.Time) time.Time {
	words := strings.Fields(text)
	if at == nil {
		now := time.Now()
		for i := range words {
			at = append(at, now.Add(time.Duration(i+1-len(words))*200*time.Millisecond))
		}
	}
	tr := voice.Transcript{Text: text, IsFinal: final, SpeechFinal: speechFinal, GateOpen: true, At: time.Now()}
	for i, w := range words {
		word := voice.Word{Text: w, Start: float64(i) * 0.2, End: float64(i)*0.2 + 0.15}
		if i < len(at) {
			word.At = at[i]
		}
		tr.Words = append(tr.Words, word)
	}
	r.sup.OnTranscript(&tr)
	return tr.Words[len(tr.Words)-1].At.Add(150 * time.Millisecond)
}

// answer runs the vendor's answer tool for question, and returns its result.
func (r *cutRig) answer(t *testing.T, id, question string) voice.AnswerResult {
	t.Helper()
	args, _ := json.Marshal(voice.AnswerArgs{Question: question})
	raw, err := r.sess.x.Tools.Run(t.Context(), voice.ToolCall{ID: id, Name: voice.AnswerTool, Args: args})
	if err != nil {
		t.Fatal(err)
	}
	var out voice.AnswerResult
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// speak makes the voice speak, and returns when.
func (r *cutRig) speak(t *testing.T) time.Time {
	t.Helper()
	if err := r.sess.x.Out.Begin().Write(t.Context(), make([]byte, 640), audio.AudioFormat{}); err != nil {
		t.Fatal(err)
	}
	r.speaker.FlushSpeech()
	return time.Now()
}

// TestABargeInKeepsItsTurnAcrossTheCut (Sol's second review of #191): a
// person who talks over a voice that drives its own turn cuts it, and goes on
// past the speaker's report of the cut. The vendor asks for the answer to the
// whole question before its final result comes. That final continues the
// turn the interruption began, since the utterance had not ended: it carries
// the answer's number, so its read-ahead searches nothing, and the question
// makes at most maxSearches searches. On 308ad445 the final took a new number
// and searched a third time. Controls: a final whose words were all said
// before the cut keeps the number too; an utterance after the voice spoke
// again takes the next one.
func TestABargeInKeepsItsTurnAcrossTheCut(t *testing.T) {
	const prefix = "what is happening with realtime voice"
	const question = prefix + " budget"
	for _, tc := range []struct {
		name     string
		afterCut bool
	}{
		{"the final continues past the cut", true},
		{"control: the final's words were all said before the cut", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newCutRig(t,
				[]*genai.GenerateContentResponse{chunk(text("ABC-12 is in progress."))},
				[]*genai.GenerateContentResponse{chunk(call("model", "search_issues", map[string]any{"query": "voice budget"}, ""))},
				[]*genai.GenerateContentResponse{chunk(text("ABC-12 is in progress."))},
			)
			r.answer(t, "first", "")
			began := r.speak(t) // the voice speaks its answer
			time.Sleep(100 * time.Millisecond)
			at := make([]time.Time, len(strings.Fields(question)))
			for i := range at {
				at[i] = began.Add(time.Duration(i+1) * 10 * time.Millisecond)
			}
			r.hear(prefix, false, at) // talks over the voice: a barge-in
			r.speaker.FlushSpeech()   // the speaker's report of the cut, before the final
			if tc.afterCut {
				at[len(at)-1] = time.Now() // the question's last word, after the cut
			}
			r.answer(t, "interrupted", question) // the vendor asks before the final
			if before := r.records.searches(); len(before) != maxSearches {
				t.Fatalf("control: the answer searched %q, want its own search and the model's", before)
			}
			r.hear(question+"?", true, at)
			time.Sleep(searchSettle + 100*time.Millisecond)
			heard, answered := r.seen.turns(question)
			if len(answered) != 2 || len(heard) < 2 || slices.ContainsFunc(heard, func(n uint64) bool { return n != answered[1] }) {
				t.Errorf("the question's Heard calls carry turns %v, its answer %v; want one number, the answer's", heard, answered)
			}
			if after := r.records.searches(); len(after) > maxSearches {
				t.Errorf("the interrupted question searched %q, want at most %d", after, maxSearches)
			}
			// Control: once the voice spoke again, the room's next words are
			// the next turn.
			next := r.speak(t)
			const later = "and when does it ship"
			r.hear(later, false, []time.Time{next.Add(10 * time.Millisecond), next.Add(20 * time.Millisecond), next.Add(30 * time.Millisecond), next.Add(40 * time.Millisecond), next.Add(50 * time.Millisecond)})
			if heard, _ := r.seen.turns(later); len(heard) != 1 || heard[0] != answered[1]+1 {
				t.Errorf("an utterance after the voice spoke again carries turns %v, want %d", heard, answered[1]+1)
			}
		})
	}
}

// TestASeparateQuestionAfterACutIsItsOwnTurn (Sol's third review of #191 at
// 71615902): a question that talks over the voice, ends its utterance and is
// answered is one turn; a separate question said after it, with no wake
// phrase and no playback between, is the next turn: its words are its own,
// its number is the next, and its read-ahead searches for it, as on main. On
// 71615902 it was glued to the first under that number, and its read-ahead
// was refused. Controls: the same question said over the voice speaking
// again, or with the wake phrase.
func TestASeparateQuestionAfterACutIsItsOwnTurn(t *testing.T) {
	const first, second = "what is happening with realtime voice budget?", "what is happening with the microphone rollout?"
	for _, tc := range []struct {
		name, said string
		speaks     bool // the voice speaks again before the second question
	}{
		{"no playback since the cut", second, false},
		{"control: said over the voice speaking again", second, true},
		{"control: said with the wake phrase", "Wispr, " + second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newCutRig(t,
				[]*genai.GenerateContentResponse{chunk(text("ABC-12 is in progress."))},
				[]*genai.GenerateContentResponse{chunk(text("ABC-12 is in progress."))},
			)
			r.answer(t, "first", "")
			r.speak(t)
			time.Sleep(time.Second) // the question is said over the voice, its words 0.2 s apart
			r.hear(first, true, nil)
			r.speaker.FlushSpeech() // the speaker's report of the cut
			r.answer(t, "interrupted", first)
			if tc.speaks {
				r.speak(t)
			}
			time.Sleep(searchSettle + 100*time.Millisecond)
			before := r.records.searches()
			time.Sleep(time.Second) // a silence, then the second question's words
			r.hear(tc.said, true, nil)
			r.speaker.FlushSpeech()
			time.Sleep(searchSettle + 150*time.Millisecond)
			heard, answered := r.seen.turns(second)
			if len(answered) != 2 || len(heard) != 1 || heard[0] != answered[1]+1 {
				t.Errorf("the second question's Heard calls carry turns %v, the answers %v; want its own, the next after %v", heard, answered, answered)
			}
			if after := r.records.searches(); !slices.Contains(after[len(before):], "what is happening with the microphone rollout") {
				t.Errorf("searches %q after the second question, %q before it; want its read-ahead to search it", after, before)
			}
		})
	}
}

// TestAStatementAfterWisprsQuestionStaysSideTalkAtAnyEndpoint (Sol's fourth
// review of #191 at 5121fa9c, from Sol's probe): after the model asks the
// room "Do you mean its status or its holder?", a voice that drives its own
// turn hears the statement "okay, ABC-12 can wait" in two results, "okay,
// ABC-12" and "can wait", and its vendor asks for an answer to the whole
// statement. Wherever an endpoint falls, the statement is side talk: nothing
// is said, searched, read or sent. On 5121fa9c three orders made "can wait"
// a turn of its own: a speech_final pause after "okay, ABC-12", and the
// opening question's late UtteranceEnd after the reply's first words, with
// or without the tail's interim before it. "Can wait?" went to the holder.
func TestAStatementAfterWisprsQuestionStaysSideTalkAtAnyEndpoint(t *testing.T) {
	const prefix, tail = "okay, ABC-12", "can wait"
	const whole = prefix + " " + tail
	for _, order := range []string{
		"control: no endpoint in the reply",
		"control: the UtteranceEnd before the reply",
		"a speech_final pause in the statement",
		"a late UtteranceEnd after the reply's first words",
		"an interim of the tail, then a late UtteranceEnd",
	} {
		t.Run(order, func(t *testing.T) {
			r := newCutRig(t,
				[]*genai.GenerateContentResponse{chunk(text("ABC-12 is in progress. Do you mean its status or its holder?"))},
				[]*genai.GenerateContentResponse{chunk(text("ABC-12 is in progress."))},
				[]*genai.GenerateContentResponse{chunk(text("ABC-12 is in progress."))},
			)
			if initial := r.answer(t, "initial", ""); !strings.HasSuffix(initial.Say, "?") {
				t.Fatalf("control: the first answer asks nothing: %q", initial.Say)
			}
			r.speak(t)
			time.Sleep(1200 * time.Millisecond)
			p0 := time.Now().Add(-time.Second) // the reply, said since the voice began
			prefixAt := []time.Time{p0, p0.Add(200 * time.Millisecond)}
			tailAt := []time.Time{p0.Add(600 * time.Millisecond), p0.Add(800 * time.Millisecond)}
			searched, read := len(r.records.searches()), len(r.records.calls())
			r.agents.mu.Lock()
			sent := len(r.agents.sent)
			r.agents.mu.Unlock()
			lateEnd := func() { r.sup.OnUtteranceEnd(0, true, r.wakeEnd, monoclock.Now()) } // the opening question's
			if order == "control: the UtteranceEnd before the reply" {
				lateEnd()
			}
			r.hearEnding(prefix, true, order == "a speech_final pause in the statement", prefixAt)
			if order == "an interim of the tail, then a late UtteranceEnd" {
				r.hearEnding(tail, false, false, tailAt)
			}
			if strings.Contains(order, "late UtteranceEnd") {
				lateEnd()
			}
			r.hearEnding(tail, true, true, tailAt)
			out := r.answer(t, "side talk", whole)
			r.agents.mu.Lock()
			messages := slices.Clone(r.agents.sent[sent:])
			r.agents.mu.Unlock()
			if searches, calls := r.records.searches()[searched:], r.records.calls()[read:]; len(searches)+len(calls)+len(messages) != 0 || out.Say != "" {
				_, answered := r.seen.turns(whole)
				t.Errorf("the statement %q was searched %q, read %q, sent %+v and said %q (answers at turns %v); want nothing", whole, searches, calls, messages, out.Say, answered)
			}
		})
	}
}

// TestTheRoomsWholeReplyDecidesSideTalk (Main's rule after Sol's fifth
// review of #191): after Wispr asked the room a question, whether the reply
// is side talk is decided on the room's own words since that question
// ended (voice.Exchange.Reply), never on a vendor's text: an endpoint inside
// a statement leaves the turn only its end ("can wait", which on its own
// asks), and the whole reply is the statement. With no whole reply, the
// turn decides. At 707af97 the whole reply went unread: "can wait" was
// answered and sent to ABC-12's holder.
//
// A reply that holds a question is answered: from the turn when the turn
// asks, picks or says a key, else from the whole reply, so "what about the
// budget?" then "okay, sure" is not answered as "okay, sure" (Main, before
// Sol's sixth review). A reply that ends by taking itself back, "okay,
// never mind", "stop" or "forget it", is answered not at all, as "Wispr,
// never mind" answers nothing; a question said after that is answered. At
// e7f3bad the question before "okay, never mind" was answered as "okay,
// never mind". A take-back is a clause that is a take-back phrase and
// nothing else: "stop the rollout?", "what about ABC-12, stop the
// rollout?" and "never mind the budget what about ABC-12?" are questions,
// which c69af0c took for take-backs and left unanswered (Sol's sixth
// review). A take-back cancels what was said before it, so "okay, sure"
// after "okay, never mind" does not bring the question back.
func TestTheRoomsWholeReplyDecidesSideTalk(t *testing.T) {
	for _, tc := range []struct {
		name, turn, whole, want string // want: "side talk", "answered" or "picked"
		asked                   string // for "answered": what the model was asked
	}{
		{"a statement's tail, the whole statement", "can wait", "okay, ABC-12 can wait", "side talk", ""},
		{"a statement's tail, its key spelled", "can wait", "Okay, A B C 12 can wait.", "side talk", ""},
		{"a statement, then a question", "What about the realtime voice budget?", "okay, ABC-12 can wait. What about the realtime voice budget?", "answered", "What about the realtime voice budget?"},
		{"a question, then a statement", "okay, sure", "what about the realtime voice budget? okay, sure", "answered", "what about the realtime voice budget? okay, sure"},
		{"a question, then okay, never mind", "okay, never mind", "what about the realtime voice budget? okay, never mind", "side talk", ""},
		{"a question, then stop", "stop.", "what about the realtime voice budget? stop.", "side talk", ""},
		{"a question, then actually, forget it", "actually, forget it", "what about the realtime voice budget? actually, forget it", "side talk", ""},
		{"a pick, then never mind", "never mind", "the first one, never mind", "side talk", ""},
		{"a question, never mind, then a statement", "okay, sure", "what about the realtime voice budget? okay, never mind. okay, sure", "side talk", ""},
		{"stop as a question's verb", "stop the rollout?", "stop the rollout?", "answered", "stop the rollout?"},
		{"stop in a question's last clause", "what about ABC-12, stop the rollout?", "what about ABC-12, stop the rollout?", "answered", "what about ABC-12, stop the rollout?"},
		{"never mind before a question, no comma", "never mind the budget what about ABC-12?", "never mind the budget what about ABC-12?", "answered", "never mind the budget what about ABC-12?"},
		{"never mind before a question, a comma", "never mind the budget, what about ABC-12?", "never mind the budget, what about ABC-12?", "answered", "never mind the budget, what about ABC-12?"},
		{"stop as a question", "stop?", "stop?", "answered", "stop?"},
		{"a question, then a clause that starts with stop", "stop the rollout.", "what about the realtime voice budget? stop the rollout.", "answered", "what about the realtime voice budget? stop the rollout."},
		{"a question, then a clause that starts with never mind", "never mind the budget.", "what about the realtime voice budget? never mind the budget.", "answered", "what about the realtime voice budget? never mind the budget."},
		{"control: a question, then that's all", "that's all.", "what about the realtime voice budget? that's all.", "side talk", ""},
		{"control: never mind, then a question", "what about the realtime voice budget?", "never mind. what about the realtime voice budget?", "answered", "what about the realtime voice budget?"},
		{"a take-back inside the turn's own final", "what about the wake word? never mind. what about the realtime voice budget?", "what about the wake word? never mind. what about the realtime voice budget?", "answered", "what about the realtime voice budget?"},
		{"a take-back inside the turn, no whole reply", "what about the wake word? never mind. what about the realtime voice budget?", "", "answered", "what about the realtime voice budget?"},
		{"control: a question, then a pick", "the first one", "what about the realtime voice budget? the first one", "picked", ""},
		{"control: the tail with no whole reply", "can wait", "", "answered", "can wait"},
		{"control: a question in one final", "what about the realtime voice budget", "what about the realtime voice budget", "answered", "what about the realtime voice budget"},
		{"control: side talk in one final", "okay, let's move on to the realtime voice budget", "okay, let's move on to the realtime voice budget", "side talk", ""},
		{"control: a pick in one final", "the first one", "the first one", "picked", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, got := answerAfterOfferReply(t, tc.turn, tc.whole)
			switch tc.want {
			case "side talk":
				if !errors.Is(got.err, voice.ErrSideTalk) || len(got.spoken) != 0 || got.full+got.keyword+got.reads+got.sent != 0 {
					t.Errorf("Answer = %v, spoken %+v, searched %d, read %d, sent %d; want side talk", got.err, got.spoken, got.full+got.keyword, got.reads, got.sent)
				}
			case "answered":
				if got.err != nil || got.took != "ABC-12" || len(got.spoken) == 0 || got.pending != 0 {
					t.Errorf("Answer = %v, took %q, spoken %+v; want the room's question answered about ABC-12", got.err, got.took, got.spoken)
				}
				if prompt := lastPrompt(r); !strings.Contains(prompt, "said after the wake word: "+tc.asked+"\n") {
					t.Errorf("the model was asked:\n%s\nwant %q", prompt, tc.asked)
				}
			default:
				if got.err != nil || got.took != "ABC-12" || got.pending != 1 {
					t.Errorf("Answer = %v, took %q, sent the question first asked %d times; want ABC-12 picked", got.err, got.took, got.pending)
				}
			}
		})
	}
}

// TestTheVendorsTextDecidesNoReplyToWisprsQuestion (Sol's fifth review of
// #191 at 707af97b, from Sol's probes): through the real Supervisor and
// Concierge, after the model asks the room "Do you mean its status or its
// holder?", whatever text the vendor sends with its answer call, the room's
// own words decide. C1: "okay, ABC-12" with a speech_final pause, then "can
// wait", with the vendor's paraphrase "Okay, ABC-12 can be postponed." or
// "Can ABC-12 wait?": at 707af97 the Concierge got "can wait" alone and sent
// "Can wait?" to ABC-12's holder; it is side talk. R1: a room question the
// vendor wraps ("The user asked …") or strips of its "?": at 707af97 the
// vendor's text vetoed it; main c1a03f3 answers it, and so must this. A
// question then "okay, never mind" in a second utterance is answered not at
// all; side talk then a question in its own utterance is answered; a
// question then a statement is answered from the whole reply, which the
// search reads (Main, before Sol's sixth review).
func TestTheVendorsTextDecidesNoReplyToWisprsQuestion(t *testing.T) {
	for _, tc := range []struct {
		name     string
		finals   []string // the room's finals, each ending its utterance when pause
		pause    bool
		vendor   string
		answer   bool
		searched string // when set, a search the answer made holds it
	}{
		{"C1: a paused statement, the vendor's statement", []string{"okay, ABC-12", "can wait"}, true, "Okay, ABC-12 can be postponed.", false, ""},
		{"C1: a paused statement, the vendor's question", []string{"okay, ABC-12", "can wait"}, true, "Can ABC-12 wait?", false, ""},
		{"control: an unpaused statement, the vendor's statement", []string{"okay, ABC-12", "can wait"}, false, "Okay, ABC-12 can be postponed.", false, ""},
		{"control: a paused statement, the vendor's own words", []string{"okay, ABC-12", "can wait"}, true, "okay, ABC-12 can wait", false, ""},
		{"control: a room statement, the vendor's question", []string{"okay, let's move on to the realtime voice budget"}, true, "What about the realtime voice budget?", false, ""},
		{"R1: a room question, the vendor's wrapper", []string{"what about the realtime voice budget"}, true, "The user asked what about the realtime voice budget.", true, ""},
		{"R1: a room question, the vendor drops its question mark", []string{"ABC-12 merged?"}, true, "ABC-12 merged.", true, ""},
		{"control: a room question, the vendor's same question", []string{"what about the realtime voice budget"}, true, "What about the realtime voice budget?", true, ""},
		{"a question, then okay, never mind", []string{"what about the realtime voice budget?", "okay, never mind"}, true, "Never mind.", false, ""},
		{"side talk, then a question as its own turn", []string{"okay, ABC-12 can wait.", "what about the realtime voice budget?"}, true, "What about the realtime voice budget?", true, ""},
		{"a question, then a statement", []string{"what about the realtime voice budget?", "okay, sure"}, true, "Okay, sure.", true, "realtime voice budget"},
		{"Sol R6: stop the rollout?", []string{"stop the rollout?"}, true, "Stop the rollout?", true, ""},
		{"Sol R6: what about ABC-12, stop the rollout?", []string{"what about ABC-12, stop the rollout?"}, true, "What about ABC-12, stop the rollout?", true, ""},
		{"Sol R6: never mind the budget what about ABC-12?", []string{"never mind the budget what about ABC-12?"}, true, "What about ABC-12?", true, ""},
		{"control: actually, stop", []string{"what about the realtime voice budget?", "actually, stop"}, true, "Stop.", false, ""},
		{"control: never mind, then a question", []string{"never mind.", "what about the realtime voice budget?"}, true, "What about the realtime voice budget?", true, ""},
		{"a question, never mind, then a statement", []string{"what about the realtime voice budget?", "okay, never mind", "okay, sure"}, true, "Okay, sure.", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newCutRig(t,
				[]*genai.GenerateContentResponse{chunk(text("ABC-12 is in progress. Do you mean its status or its holder?"))},
				[]*genai.GenerateContentResponse{chunk(text("ABC-12 is in progress."))},
				[]*genai.GenerateContentResponse{chunk(text("ABC-12 is in progress."))},
			)
			if initial := r.answer(t, "initial", ""); !strings.HasSuffix(initial.Say, "?") {
				t.Fatalf("control: the first answer asks nothing: %q", initial.Say)
			}
			r.speak(t)
			time.Sleep(1200 * time.Millisecond)
			p0 := time.Now().Add(-time.Second) // the reply, said since the voice began
			searched, read := len(r.records.searches()), len(r.records.calls())
			r.agents.mu.Lock()
			sent := len(r.agents.sent)
			r.agents.mu.Unlock()
			for i, final := range tc.finals {
				at := []time.Time{}
				for j := range strings.Fields(final) {
					at = append(at, p0.Add(time.Duration(i)*600*time.Millisecond+time.Duration(j)*100*time.Millisecond))
				}
				r.hearEnding(final, true, tc.pause || i == len(tc.finals)-1, at)
			}
			out := r.answer(t, "reply", tc.vendor)
			r.agents.mu.Lock()
			messages := slices.Clone(r.agents.sent[sent:])
			r.agents.mu.Unlock()
			searches, calls := r.records.searches()[searched:], r.records.calls()[read:]
			if tc.answer {
				if out.Say == "" {
					t.Errorf("the room's question was not answered: searched %q, read %q, sent %+v", searches, calls, messages)
				}
				if tc.searched != "" && !slices.ContainsFunc(searches, func(s string) bool { return strings.Contains(s, tc.searched) }) {
					t.Errorf("the answer searched %q, want a search for %q", searches, tc.searched)
				}
				return
			}
			if len(searches)+len(calls)+len(messages) != 0 || out.Say != "" {
				t.Errorf("the room's statement was searched %q, read %q, sent %+v and said %q; want nothing", searches, calls, messages, out.Say)
			}
		})
	}
}

// askedBackRig is a cutRig after the model asked the room "Do you mean its
// status or its holder?", its voice speaking, with ABC-42, "A different
// issue", in the record beside ABC-12; the model answers each later turn.
func askedBackRig(t *testing.T) *cutRig {
	t.Helper()
	turns := [][]*genai.GenerateContentResponse{{chunk(text("ABC-12 is in progress. Do you mean its status or its holder?"))}}
	for range 4 {
		turns = append(turns, []*genai.GenerateContentResponse{chunk(text("ABC-12 is in progress."))})
	}
	r := newCutRig(t, turns...)
	r.records.mu.Lock()
	r.records.issues["ABC-42"] = &dispatch.Issue{Key: "ABC-42", Title: "A different issue", Status: "todo", UpdatedAt: testNow}
	r.records.mu.Unlock()
	if initial := r.answer(t, "initial", ""); !strings.HasSuffix(initial.Say, "?") {
		t.Fatalf("control: the first answer asks nothing: %q", initial.Say)
	}
	r.speak(t)
	return r
}

// TestAKeySplitAcrossFinalsIsReadWhole (Sol's seventh review of #191 at
// 5611ca1a, TestSol191R7NumberSplit): after Wispr's question, "ABC forty"
// and "two" in two finals are the key ABC-42, as main reads them, whether
// an endpoint falls between them or not; so are "A B C" and "forty two". At
// 5611ca1 the reply wrote keys final by final, "ABC-40\ntwo", and took the
// key for side talk: nothing was read or said. Across an endpoint the turn
// is "two" alone, so the answer reads the reply, a key said alone. Keys are
// written over the whole reply, as over a turn; final ends only mark where
// a take-back ends (Main).
func TestAKeySplitAcrossFinalsIsReadWhole(t *testing.T) {
	for _, tc := range []struct {
		name      string
		finals    []string
		endpoints bool
	}{
		{"ABC forty / two, no endpoint between", []string{"ABC forty", "two"}, false},
		{"ABC forty / two, across an endpoint", []string{"ABC forty", "two"}, true},
		{"A B C / forty two, across an endpoint", []string{"A B C", "forty two"}, true},
		{"control: ABC forty two, one final", []string{"ABC forty two"}, false},
		{"control: ABC-42, one final", []string{"ABC-42"}, false},
		{"control: A B C forty two, one final", []string{"A B C forty two"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := askedBackRig(t)
			read := len(r.records.calls())
			for i, final := range tc.finals {
				r.hearEnding(final, true, tc.endpoints || i == len(tc.finals)-1, nil)
			}
			out := r.answer(t, "split key", strings.Join(tc.finals, " "))
			calls := r.records.calls()[read:]
			if !slices.Contains(calls, "ABC-42") || out.IssueKey != "ABC-42" || out.Say == "" {
				t.Errorf("read %q, took %q, said %q; want ABC-42 read and answered", calls, out.IssueKey, out.Say)
			}
		})
	}
}

// TestATakeBackCancelsWhatCameBeforeItInTheSameFinal (Sol's seventh review
// of #191, TestSol191R7CanceledKey): "what about ABC-42? never mind. what
// about ABC-12?" in one final answers only "what about ABC-12?": ABC-42 is
// neither read nor named to the holder or the model. At 5611ca1 the whole
// final went to the model and the holder; main does the same. Controls: the
// question alone; the take-back in a final of its own; the same final then
// "okay sure", answered from what follows the take-back.
//
// The rows with no endpoint come from Sol's eighth review of #191 (his
// TestSol191R8RawCancellationBoundary). When the end of a final, not a stop,
// ends the take-back, and no endpoint ends the turn there, the turn joins
// the finals with a space ("what about ABC-42? never mind what about
// ABC-12?"), where "never mind" is no clause of its own. At 720839d the
// answer cut that joined turn, found no take-back, and sent the holder "What
// about ABC-42? never mind what about ABC-12?". The answer text is the cut
// the raw finals give.
func TestATakeBackCancelsWhatCameBeforeItInTheSameFinal(t *testing.T) {
	for _, tc := range []struct {
		name      string
		finals    []string
		endpoints bool // every final ends its utterance, not only the last
	}{
		{"one final", []string{"what about ABC-42? never mind. what about ABC-12?"}, true},
		{"control: the question alone", []string{"what about ABC-12?"}, true},
		{"control: the take-back a final of its own", []string{"what about ABC-42?", "never mind", "what about ABC-12?"}, true},
		{"control: one final, then okay sure", []string{"what about ABC-42? never mind. what about ABC-12?", "okay sure"}, true},
		{"a take-back ends its final, no endpoint", []string{"what about ABC-42? never mind", "what about ABC-12?"}, false},
		{"a take-back a final of its own, no endpoint", []string{"what about ABC-42?", "never mind", "what about ABC-12?"}, false},
		{"a comma, a take-back ends its final, no endpoint", []string{"what about ABC-42, never mind", "what about ABC-12?"}, false},
		{"control: one final, no endpoint", []string{"what about ABC-42? never mind. what about ABC-12?"}, false},
		{"control: a take-back ends its final, an endpoint", []string{"what about ABC-42? never mind", "what about ABC-12?"}, true},
		{"control: a stop after the take-back, no endpoint", []string{"what about ABC-42? never mind.", "what about ABC-12?"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := askedBackRig(t)
			read := len(r.records.calls())
			r.agents.mu.Lock()
			sent := len(r.agents.sent)
			r.agents.mu.Unlock()
			for i, final := range tc.finals {
				r.hearEnding(final, true, tc.endpoints || i == len(tc.finals)-1, nil)
			}
			out := r.answer(t, "take-back", strings.Join(tc.finals, " "))
			r.agents.mu.Lock()
			messages := slices.Clone(r.agents.sent[sent:])
			r.agents.mu.Unlock()
			if calls := r.records.calls()[read:]; slices.Contains(calls, "ABC-42") {
				t.Errorf("the cancelled issue was read: %q", calls)
			}
			for _, m := range messages {
				if strings.Contains(m.Message, "ABC-42") {
					t.Errorf("the holder was sent the cancelled question: %q", m.Message)
				}
			}
			if prompt := lastPrompt(r.rig); strings.Contains(prompt, "ABC-42") {
				t.Errorf("the model was given the cancelled question:\n%s", prompt)
			}
			if out.IssueKey != "ABC-12" || out.Say == "" {
				t.Errorf("took %q, said %q; want the question after the take-back answered about ABC-12", out.IssueKey, out.Say)
			}
		})
	}
}

// TestASplitKeyTheRecordLacksIsHeardAsNoIssue (Sol's seventh review of #191,
// TestSol191R7SplitKeys's "ABC ten" / "two" rows): the reply's keys are
// written over the whole reply (Main's rule), so "ABC ten" and "two" are
// ABC-102, as one final "ABC ten two" is. The record has no ABC-102: the
// room hears "I found no issue for that. What is its key?", which the voice
// speaks itself as the answer's lead line, so the answer tool's own words
// are empty. Main read the first final's ABC-10 instead. Control: the same
// words in one final.
func TestASplitKeyTheRecordLacksIsHeardAsNoIssue(t *testing.T) {
	for _, tc := range []struct {
		name      string
		finals    []string
		endpoints bool
	}{
		{"ABC ten / two, no endpoint between", []string{"ABC ten", "two"}, false},
		{"ABC ten / two, across an endpoint", []string{"ABC ten", "two"}, true},
		{"control: ABC ten two, one final", []string{"ABC ten two"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := askedBackRig(t)
			read := len(r.records.calls())
			for i, final := range tc.finals {
				r.hearEnding(final, true, tc.endpoints || i == len(tc.finals)-1, nil)
			}
			r.answer(t, "split key", strings.Join(tc.finals, " "))
			if calls := r.records.calls()[read:]; !slices.Contains(calls, "ABC-102") {
				t.Errorf("read %q, want ABC-102", calls)
			}
			r7Until(t, "the no-issue line spoken", func() bool {
				return slices.ContainsFunc(r.driver.said(), func(s r7Said) bool { return s.text == noIssueFound })
			})
		})
	}
}

// TestWhereTheTwoRulesMeet (Main, on rebasing #191 onto #194): #194's rules
// and #191's meet where a reply to Wispr's question holds a decline, a key
// of one digit, a spelled key or more keys than a read tries. It fails if
// "no, not ABC-12" after an offer is no decline, or leaves a read a next
// question takes; if a key of one digit, or a spelled offered key, said
// after a take-back is not the one read; or if a key said before a take-back
// is read while the keys after it are held to the read's limit.
func TestWhereTheTwoRulesMeet(t *testing.T) {
	t.Run("no, not ABC-12 after an offer", func(t *testing.T) {
		r, got := answerAfterOfferReply(t, "no, not ABC-12", "no, not ABC-12")
		if got.err != nil || len(got.spoken) != 1 || got.spoken[0].text != "Okay." || got.reads+got.full+got.keyword+got.sent != 0 {
			t.Fatalf("Answer = %v, spoken %+v, read %d, searched %d, sent %d; want only \"Okay.\"", got.err, got.spoken, got.reads, got.full+got.keyword, got.sent)
		}
		r.exchange.Reply = ""
		// No search finds an issue: only a read of the declined key could.
		r.records.mu.Lock()
		r.records.hits, r.records.kwHits = nil, nil
		r.records.mu.Unlock()
		var spoken []said
		err := r.concierge.Answer(context.Background(), &r.exchange, "ask the agent whether the schedule is ready", recorder(&spoken))
		if r.exchange.IssueKey == "ABC-12" {
			t.Errorf("the next question named the declined ABC-12 (Answer = %v, spoken %+v)", err, spoken)
		}
	})
	// OPS is a project the list does not hold: only the offer's own view
	// (offeredView) reads "O P S thirteen" as OPS-13. The keys of the limit's
	// row name no issue, so a read tries each of them.
	opsOffered := []candidate{{"OPS-13", "Wake word"}, {"OPS-14", "Realtime voice"}}
	for _, tc := range []struct {
		name, reply, want string
		cancelled         string
		offered           []candidate
	}{
		{"a key of one digit after a take-back", "what about ABC-42? never mind. what about INFRA-7?", "INFRA-7", "ABC-42", twoOffered},
		{"a key of one digit said in words after a take-back", "what about ABC-42? never mind. what about infra seven?", "INFRA-7", "ABC-42", twoOffered},
		{"an offered key spelled after a take-back", "what about ABC-42? never mind. A B C thirteen", "ABC-13", "ABC-42", twoOffered},
		{"an offered key of a project the list lacks, spelled after a take-back", "what about ABC-42? never mind. O P S thirteen", "OPS-13", "ABC-42", opsOffered},
		{"more keys than a read tries after a take-back", "what about XYZ-15? never mind. what about ABC-97 and ABC-98 and DEF-99 and XYZ-96?", "", "XYZ-15", twoOffered},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			r.exchange.Reply = tc.reply
			heldIssue(r, "session", false)
			oneDigitIssue(r)
			r.records.mu.Lock()
			for _, key := range []string{"ABC-13", "ABC-42", "OPS-13", "OPS-14", "XYZ-15"} {
				r.records.issues[key] = &dispatch.Issue{Key: key, Title: "Recorder " + key, Status: "todo", UpdatedAt: testNow}
			}
			r.records.mu.Unlock()
			r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("It is in progress."))}}
			r.concierge.offerPick(&r.exchange, twoOfferedTurn, tc.offered)
			var spoken []said
			err := r.concierge.Answer(context.Background(), &r.exchange, tc.reply, recorder(&spoken))
			calls := r.records.calls()
			if slices.Contains(calls, tc.cancelled) || strings.Contains(lastPrompt(r), tc.cancelled) {
				t.Errorf("the cancelled %s was read or given to the model: reads %q", tc.cancelled, calls)
			}
			r.agents.mu.Lock()
			for _, m := range r.agents.sent {
				if strings.Contains(m.Message, tc.cancelled) {
					t.Errorf("a holder was sent the cancelled %s: %q", tc.cancelled, m.Message)
				}
			}
			r.agents.mu.Unlock()
			if tc.want != "" && r.exchange.IssueKey != tc.want {
				t.Errorf("Answer = %v, took %q, spoken %+v, reads %q; want %s", err, r.exchange.IssueKey, spoken, calls, tc.want)
			}
			tried := slices.Clone(calls)
			slices.Sort(tried)
			if tried = slices.Compact(tried); len(tried) > maxKeys {
				t.Errorf("a read tried %q, want at most %d keys", tried, maxKeys)
			}
		})
	}
}
