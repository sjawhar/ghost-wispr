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
	// wakeEnd is when the exchange's opening question's last word ended,
	// by capture: the position of its UtteranceEnd.
	wakeEnd time.Time
}

// newCutRig opens an exchange with "Wispr, what is happening with ABC-12?";
// the model answers with turns.
func newCutRig(t *testing.T, turns ...[]*genai.GenerateContentResponse) *cutRig {
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
	r.sup = voice.NewSupervisor(&voice.SupervisorConfig{
		Voice:        func() (voice.Voice, error) { return driver, nil },
		Concierge:    r.seen,
		Speaker:      r.speaker,
		WakePhrases:  []string{"Wispr"},
		Session:      func() string { return "meeting-1" },
		Red:          func() bool { return true },
		IdleWait:     time.Minute,
		FollowUpWait: time.Minute,
		Logf:         t.Logf,
	})
	t.Cleanup(r.sup.Close)
	r.wakeEnd = r.hear("Wispr, what is happening with ABC-12?", true, nil)
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

// TestTheVendorsReadingOfAReplyCanOnlyMakeItSideTalk (Main's rule after Sol's
// fourth review of #191): a reply to Wispr's question is side talk when any
// reading of it is, the room's words or the vendor's own text
// (voice.Exchange.VendorReading) when that text holds the room's words, in
// order, whatever their case and punctuation: nothing is said, read,
// searched or sent. It is picked and answered from the room's words alone. A
// vendor's text that does not hold the room's words, a rephrasing, is not a
// reading: a real question the vendor rephrased as a statement is answered.
func TestTheVendorsReadingOfAReplyCanOnlyMakeItSideTalk(t *testing.T) {
	for _, tc := range []struct {
		name, reply, vendor, want string // want: "side talk", "answered" or "picked"
	}{
		{"the room heard only a statement's tail", "can wait", "okay, ABC-12 can wait", "side talk"},
		{"the room heard a spelled key's statement's tail", "can wait", "Okay, A B C 12 can wait.", "side talk"},
		{"the vendor's statement, the room's question", "what about the realtime voice budget", "Tell me about the realtime voice budget.", "answered"},
		{"the vendor's longer statement, the room's question", "what about the realtime voice budget", "Okay, so the user wants to know about the realtime voice budget.", "answered"},
		{"the room's side talk, the vendor's question", "okay, let's move on to the realtime voice budget", "What about the realtime voice budget?", "side talk"},
		{"control: a question, both readings", "what about the realtime voice budget", "What about the realtime voice budget?", "answered"},
		{"control: a pick, both readings", "the first one", "The first one.", "picked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, got := answerAfterOfferRead(t, tc.reply, tc.vendor)
			switch tc.want {
			case "side talk":
				if !errors.Is(got.err, voice.ErrSideTalk) || len(got.spoken) != 0 || got.full+got.keyword+got.reads+got.sent != 0 {
					t.Errorf("Answer = %v, spoken %+v, searched %d, read %d, sent %d; want side talk", got.err, got.spoken, got.full+got.keyword, got.reads, got.sent)
				}
			case "answered":
				if got.err != nil || got.took != "ABC-12" || len(got.spoken) == 0 || got.pending != 0 {
					t.Errorf("Answer = %v, took %q, spoken %+v; want the room's question answered about ABC-12", got.err, got.took, got.spoken)
				}
			default:
				if got.err != nil || got.took != "ABC-12" || got.pending != 1 {
					t.Errorf("Answer = %v, took %q, sent the question first asked %d times; want ABC-12 picked", got.err, got.took, got.pending)
				}
			}
		})
	}
}
