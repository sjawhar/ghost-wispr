package concierge

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/dispatch"
	"github.com/sjawhar/ghost-wispr/internal/envoy"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// Four defects found by reading main at c57adc3 against R5's rewrite: side
// talk after an offer reached an agent, a zero score was a clear match, two
// outages of Dispatch's were heard as "I found no issue", and searches made
// while a question was asked went uncounted.

// replyTurn answers reply, a turn after an offer, and returns what was said.
// It fails t on any error but side talk (voice.ErrSideTalk), which says
// nothing.
func replyTurn(t *testing.T, r *rig, reply string) []said {
	t.Helper()
	var spoken []said
	if err := r.concierge.Answer(context.Background(), &r.exchange, reply, recorder(&spoken)); err != nil && !errors.Is(err, voice.ErrSideTalk) {
		t.Fatalf("Answer(%q): %v", reply, err)
	}
	return spoken
}

// offerOutcome is what an answer to a reply after an offer did, as the room,
// Dispatch and the holder saw it.
type offerOutcome struct {
	spoken        []said
	err           error
	full, keyword int
	reads         int
	sent, pending int
	took          string
}

// answerAfterOffer offers ABC-12 and ABC-13 for twoOfferedTurn, ABC-12
// ("Realtime voice", held by the live session agent-7) every search's one
// clear match, then hears reply as the read-ahead does, past its settle, and
// answers it. pending counts the sends of the question first asked.
func answerAfterOffer(t *testing.T, reply string) (*rig, offerOutcome) {
	t.Helper()
	return answerAfterOfferReply(t, reply, "")
}

// answerAfterOfferReply is answerAfterOffer with whole, the room's words
// since Wispr's question, through the turn reply (voice.Exchange.Reply).
func answerAfterOfferReply(t *testing.T, reply, whole string) (*rig, offerOutcome) {
	t.Helper()
	r := newRig(t)
	r.exchange.Reply = whole
	r.concierge.searchSettle = 20 * time.Millisecond
	heldIssue(r, "session", false)
	r.records.mu.Lock()
	r.records.issues["ABC-13"] = &dispatch.Issue{Key: "ABC-13", Title: "Wake word", Status: "todo", UpdatedAt: testNow}
	r.records.hits = []dispatch.SearchHit{{Kind: "issue", IssueKey: "ABC-12", Title: "Realtime voice", Score: 0.9}}
	r.records.kwHits = slices.Clone(r.records.hits)
	r.records.mu.Unlock()
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("ABC-12: PR 80 is in review."))}}
	r.concierge.offerPick(&r.exchange, twoOfferedTurn, twoOffered)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.concierge.Heard(ctx, &r.exchange, reply)
	time.Sleep(100 * time.Millisecond) // the read-ahead's settle passes
	var got offerOutcome
	got.err = r.concierge.Answer(ctx, &r.exchange, reply, recorder(&got.spoken))
	got.took = r.exchange.IssueKey
	r.records.mu.Lock()
	got.full, got.keyword, got.reads = len(r.records.queries), len(r.records.kwQueries), len(r.records.issueCalls)
	r.records.mu.Unlock()
	r.agents.mu.Lock()
	defer r.agents.mu.Unlock()
	got.sent = len(r.agents.sent)
	for _, message := range r.agents.sent {
		if strings.Contains(message.Message, "Is the tap merged?") {
			got.pending++
		}
	}
	return r, got
}

// TestSideTalkAfterAnOfferIsNotAnswered (R5, Main's rule on R5's rewrite):
// after "Did you mean …?", a reply that picks no offered issue is a new
// question only when it is a question. Side talk, a missed pick and a key
// not offered among other words are none: the offer ends, nothing is said,
// read, searched or sent, and Answer reports side talk (voice.ErrSideTalk),
// so the Supervisor closes the exchange as it closes side talk in the
// follow-up window. On main the first was sent to the holder as a question.
// A question's verb or word inside a clause ("we can move on", "let's see
// what it is", "okay, ABC-12 can wait") does not make talk a question; only
// one that starts a clause does, and an issue key is a word of the clause
// (Sol's review of #191 at 1cbcb494).
func TestSideTalkAfterAnOfferIsNotAnswered(t *testing.T) {
	for _, reply := range []string{
		"okay, let's move on to the realtime voice budget",
		"I'd say the realtime voice one",
		"right, so the budget",
		"okay, on to ABC-99 and the rollout then",
		"okay, we can move on to the realtime voice budget",
		"that was the realtime voice one",
		"let's see what the realtime voice budget is",
		"so we can move on to the budget",
		"okay, the realtime voice is done",
		"and we do have the budget",
		"but we can discuss ABC-12 later",
		"right, we can move on",
		"so ABC-12 is merged",
		"okay, ABC-12 can wait",
		"and ABC-12 does work",
		"but ABC-12 is done",
		"right, ABC-12 can wait",
	} {
		t.Run(reply, func(t *testing.T) {
			r, got := answerAfterOffer(t, reply)
			if len(got.spoken) != 0 || !errors.Is(got.err, voice.ErrSideTalk) {
				t.Errorf("Answer = %v, spoken %+v; want nothing said and voice.ErrSideTalk", got.err, got.spoken)
			}
			if got.full+got.keyword+got.reads != 0 || got.sent != 0 || got.took != "" {
				t.Errorf("searched %d full and %d keyword, read %d issues, sent %d, took %q; want nothing", got.full, got.keyword, got.reads, got.sent, got.took)
			}
			if _, key, ok := r.concierge.takePick(&r.exchange, "the first one"); ok || key != "" {
				t.Errorf("the offer still picks %q after side talk; want it ended", key)
			}
		})
	}
}

// TestSideTalkAfterWhatIsItsKeyIsNotAnswered (R5, the same rule): after "I
// found no issue for that. What is its key?", a reply that is no question
// and says no key alone is side talk: nothing is said, searched or sent. A
// key said for the question ("ABC-12", "it's ABC-12") answers the question
// first asked, about that key, and sends the holder no "ABC-12?". A question
// in its place is answered as itself.
func TestSideTalkAfterWhatIsItsKeyIsNotAnswered(t *testing.T) {
	const asked = "what is happening with the purple submarine"
	for _, tc := range []struct {
		reply, want string // want: "side talk", "the key" or "the question"
	}{
		{"okay, let's move on to the realtime voice budget", "side talk"},
		{"right, so the budget", "side talk"},
		{"okay, let's move on to the 2026 realtime voice budget", "side talk"},
		{"right, we spent 500 on the realtime voice", "side talk"},
		{"ABC-12", "the key"},
		{"it's ABC-12", "the key"},
		{"what about the realtime voice", "the question"},
	} {
		t.Run(tc.reply, func(t *testing.T) {
			r := newRig(t)
			heldIssue(r, "session", false)
			r.records.hits = nil
			if spoken := answerTurn(t, r, asked); len(spoken) != 1 || spoken[0].text != noIssueFound {
				t.Fatalf("spoken %+v, want %q", spoken, noIssueFound)
			}
			r.records.mu.Lock()
			r.records.hits = []dispatch.SearchHit{{Kind: "issue", IssueKey: "ABC-12", Title: "Realtime voice", Score: 0.9}}
			r.records.kwHits = slices.Clone(r.records.hits)
			searchedBefore := len(r.records.queries) + len(r.records.kwQueries)
			r.records.mu.Unlock()
			r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("ABC-12 is in progress."))}}
			var spoken []said
			err := r.concierge.Answer(context.Background(), &r.exchange, tc.reply, recorder(&spoken))
			r.records.mu.Lock()
			searched := len(r.records.queries) + len(r.records.kwQueries) - searchedBefore
			r.records.mu.Unlock()
			r.agents.mu.Lock()
			sent := slices.Clone(r.agents.sent)
			r.agents.mu.Unlock()
			switch tc.want {
			case "side talk":
				if !errors.Is(err, voice.ErrSideTalk) || len(spoken) != 0 || searched != 0 || len(sent) != 0 {
					t.Errorf("Answer = %v, spoken %+v, searched %d, sent %+v; want side talk: nothing said, searched or sent", err, spoken, searched, sent)
				}
				return
			case "the key":
				if prompt := lastPrompt(r); err != nil || r.exchange.IssueKey != "ABC-12" || !strings.Contains(prompt, "said after the wake word: "+asked) || searched != 0 {
					t.Errorf("Answer = %v, took %q, searched %d, prompt:\n%s\nwant the question first asked answered about ABC-12, no search", err, r.exchange.IssueKey, searched, prompt)
				}
				if len(sent) != 0 {
					t.Errorf("sent the holder %+v, want nothing: the question first asked is about status and the record is fresh", sent)
				}
			default:
				if prompt := lastPrompt(r); err != nil || r.exchange.IssueKey != "ABC-12" || !strings.Contains(prompt, "said after the wake word: "+tc.reply) {
					t.Errorf("Answer = %v, took %q, prompt:\n%s\nwant the reply answered as its own question about ABC-12", err, r.exchange.IssueKey, prompt)
				}
			}
		})
	}
}

// TestANumberAfterWhatIsItsKeyKeepsTheQuestion: a number said alone for
// "What is its key?" asks which issue it names, for the question first
// asked; the yes answers that question about the issue, not "4592".
func TestANumberAfterWhatIsItsKeyKeepsTheQuestion(t *testing.T) {
	const asked = "what is happening with the purple submarine"
	r := newRig(t)
	r.records.hits = nil
	r.records.issues["INFRA-4592"] = &dispatch.Issue{Key: "INFRA-4592", Title: "Realtime voice", Status: "in_progress", UpdatedAt: testNow}
	if spoken := answerTurn(t, r, asked); len(spoken) != 1 || spoken[0].text != noIssueFound {
		t.Fatalf("spoken %+v, want %q", spoken, noIssueFound)
	}
	if spoken := answerTurn(t, r, "4592"); len(spoken) != 1 || spoken[0].text != "Did you mean Infra forty-five ninety-two?" {
		t.Fatalf("spoken %+v, want which issue 4592 names", spoken)
	}
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("INFRA-4592 is in progress."))}}
	answerTurn(t, r, "yes")
	if prompt := lastPrompt(r); r.exchange.IssueKey != "INFRA-4592" || !strings.Contains(prompt, "said after the wake word: "+asked) {
		t.Errorf("took %q, prompt:\n%s\nwant the question first asked answered about INFRA-4592", r.exchange.IssueKey, prompt)
	}
}

// lastPrompt is the first part of the model's last request, its prompt; ""
// when the model was not called.
func lastPrompt(r *rig) string {
	r.model.mu.Lock()
	defer r.model.mu.Unlock()
	if len(r.model.requests) == 0 {
		return ""
	}
	return r.model.requests[len(r.model.requests)-1][0].Parts[0].Text
}

// TestAQuestionOrAPickAfterAnOfferIsAnswered: the controls of the rule
// above. A question after an offer, by its question mark or by a question
// word, is answered as a new question; a pick still picks and sends the
// question first asked.
func TestAQuestionOrAPickAfterAnOfferIsAnswered(t *testing.T) {
	for _, reply := range []string{
		"what is the realtime voice budget?",
		"what about the realtime voice budget",
		"okay, what about the realtime voice budget",
		"is the realtime voice budget done",
		"and when does the realtime voice budget land",
		"is ABC-12 merged",
		"and the second one?",
	} {
		t.Run(reply, func(t *testing.T) {
			_, got := answerAfterOffer(t, reply)
			if got.err != nil || got.took != "ABC-12" || !slices.ContainsFunc(got.spoken, func(s said) bool { return s.text == "ABC-12: PR 80 is in review." }) {
				t.Errorf("Answer = %v, took %q, spoken %+v; want the question answered about ABC-12", got.err, got.took, got.spoken)
			}
			if got.pending != 0 {
				t.Errorf("sent the question first asked %d times, want none", got.pending)
			}
		})
	}
	t.Run("the first one", func(t *testing.T) {
		_, got := answerAfterOffer(t, "the first one")
		if got.err != nil || got.took != "ABC-12" || got.pending != 1 {
			t.Errorf("Answer = %v, took %q, sent the question first asked %d times; want ABC-12 picked and it sent once", got.err, got.took, got.pending)
		}
	})
}

// TestSideTalkAfterAnOfferThroughTheSupervisor: through the real
// Supervisor, after the bare number's "Did you mean …?", side talk gets no
// word, no sorry and no error sound, reaches neither Dispatch nor the
// holder, and closes the exchange; a question in its place is answered.
func TestSideTalkAfterAnOfferThroughTheSupervisor(t *testing.T) {
	for _, tc := range []struct {
		reply    string
		answered bool
	}{
		{"okay, let's move on to the realtime voice budget", false},
		{"what about the realtime voice budget", true},
	} {
		t.Run(tc.reply, func(t *testing.T) {
			r := newR7Rig(t)
			records := r.c.records.(*fakeRecords)
			records.mu.Lock()
			records.issues["ABC-4592"] = &dispatch.Issue{Key: "ABC-4592", Title: "Rollout plan", Status: "in_progress", UpdatedAt: time.Now(), Claim: &dispatch.Claim{Actor: actor("session", "agent-7", "Worker")}}
			records.mu.Unlock()
			r.hear("Wispr, ask him for 4592")
			r7Until(t, "confirmation spoken", func() bool {
				return slices.ContainsFunc(r.voice.said(), func(s r7Said) bool { return strings.HasPrefix(s.text, "Did you mean ") })
			})
			confirmed := len(r.voice.said())
			records.mu.Lock()
			records.hits = []dispatch.SearchHit{{Kind: "issue", IssueKey: "ABC-12", Title: "Realtime voice", Score: 0.9}}
			records.kwHits = slices.Clone(records.hits)
			searchedBefore := len(records.queries) + len(records.kwQueries)
			records.mu.Unlock()
			time.Sleep(20 * time.Millisecond)
			r.hear(tc.reply)
			r7Until(t, "the exchange ended", func() bool { opened, closed := r.voice.counts(); return opened == 1 && closed == 1 })
			after := r.voice.said()[confirmed:]
			records.mu.Lock()
			searched := len(records.queries) + len(records.kwQueries) - searchedBefore
			records.mu.Unlock()
			r.agents.mu.Lock()
			sent := len(r.agents.sent)
			r.agents.mu.Unlock()
			if tc.answered {
				if len(after) == 0 {
					t.Errorf("a question after the offer got no answer")
				}
				return
			}
			if len(after) != 0 || searched != 0 || sent != 0 {
				t.Errorf("after side talk the voice said %+v, searched %d times, sent %d; want nothing", after, searched, sent)
			}
			if sounds := r.speaker.sounds(voice.ErrorChime()); len(sounds) != 0 {
				t.Errorf("side talk played the error sound %d times, want none", len(sounds))
			}
		})
	}
}

// TestAZeroScoreIsNoClearMatch (R5): one clear match needs a best issue
// that scores above zero and leads every other by clearMatchRatio. Scores of
// zero lead nothing: on main two of them, or one alone, were a clear match,
// and the voice took an issue it had no rank for.
func TestAZeroScoreIsNoClearMatch(t *testing.T) {
	for _, tc := range []struct {
		scores []float64
		want   bool
	}{
		{[]float64{0}, false},
		{[]float64{0, 0}, false},
		{nil, false},
		{[]float64{0.9}, true},
		{[]float64{0.9, 0.5}, true},
		{[]float64{0.9, 0.7}, false},
	} {
		if got := clearMatch(tc.scores); got != tc.want {
			t.Errorf("clearMatch(%v) = %v, want %v", tc.scores, got, tc.want)
		}
	}
	t.Run("through Answer", func(t *testing.T) {
		r := newRig(t)
		heldIssue(r, "session", false)
		r.records.issues["ABC-13"] = &dispatch.Issue{Key: "ABC-13", Title: "Wake word", Status: "todo"}
		r.records.hits = []dispatch.SearchHit{{Kind: "issue", IssueKey: "ABC-12", Title: "Realtime voice"}, {Kind: "issue", IssueKey: "ABC-13", Title: "Realtime wake word"}}
		r.records.kwHits = slices.Clone(r.records.hits)
		r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("ABC-12 is in progress."))}}
		var spoken []said
		err := r.concierge.Answer(context.Background(), &r.exchange, "what is happening with the realtime voice", recorder(&spoken))
		if err != nil || len(spoken) != 1 || spoken[0].text != "Did you mean A B C twelve, or A B C thirteen?" || r.exchange.IssueKey != "" {
			t.Errorf("Answer = %v, spoken %+v, took %q; want only which of the two", err, spoken, r.exchange.IssueKey)
		}
	})
}

// TestAFailedKeywordSearchAndASlowFullSearchAreAnOutage (R10): when the
// keyword search fails and the full search answers nothing in its bound,
// Dispatch failed a search: the room hears that Dispatch is out of reach,
// with the error sound, as for any other outage of Dispatch's. On main it
// heard "I found no issue for that". A keyword search that answered, with
// nothing, beside a slow full one is a slow search: the room hears so, with
// no error sound (#194's rule: a slow search is no outage, a failed one is).
func TestAFailedKeywordSearchAndASlowFullSearchAreAnOutage(t *testing.T) {
	for name, kwErr := range map[string]error{
		"out of reach": fmt.Errorf("dispatch: %w", dispatch.ErrUnreachable),
		"refused":      &dispatch.APIError{Status: 503, Code: "UNAVAILABLE", Route: "search"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			voiceIssue(r)
			r.concierge.searchBound = 100 * time.Millisecond
			r.records.kwErr = kwErr
			r.records.searchDelay = time.Hour
			var spoken []said
			err := r.concierge.Answer(context.Background(), &r.exchange, "what is happening with the realtime voice", recorder(&spoken))
			if !errors.Is(err, voice.ErrUnreachable) || len(spoken) == 0 || spoken[len(spoken)-1].text != cannotReachDispatch {
				t.Errorf("Answer = %v, spoken %+v; want %q and voice.ErrUnreachable", err, spoken, cannotReachDispatch)
			}
		})
	}
	t.Run("control: the keyword search answered nothing", func(t *testing.T) {
		r := newRig(t)
		voiceIssue(r)
		r.concierge.searchBound = 100 * time.Millisecond
		r.records.kwHits = nil
		r.records.searchDelay = time.Hour
		var spoken []said
		err := r.concierge.Answer(context.Background(), &r.exchange, "what is happening with the realtime voice", recorder(&spoken))
		if err != nil || len(spoken) == 0 || spoken[len(spoken)-1].text != searchIsSlow {
			t.Errorf("Answer = %v, spoken %+v; want %q and no error", err, spoken, searchIsSlow)
		}
	})
}

// TestANumberAloneDispatchCannotLookUpIsAnOutage (R10): a number said with
// no other words to search ("4592") is read in every project Dispatch has.
// When that cannot be done, because the project list was never read or
// Dispatch did not answer the reads, the room hears that Dispatch is out of
// reach, with the error sound, and nothing is searched; on main it heard "I
// found no issue for that". A number Dispatch answered no project has is
// still no issue found.
func TestANumberAloneDispatchCannotLookUpIsAnOutage(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state projectsState
		turn  string
	}{
		{"the list never read, its read hangs", projectsNeverRead, "4592"},
		{"the list never read, its read fails", projectsReadFails, "4592"},
		{"the list never read, a word that names no work", projectsReadFails, "4592, please"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			r.records.issues["INFRA-4592"] = &dispatch.Issue{Key: "INFRA-4592", Title: "Realtime voice", Status: "in_progress", UpdatedAt: testNow}
			tc.state.set(t, r)
			var spoken []said
			err := r.concierge.Answer(context.Background(), &r.exchange, tc.turn, recorder(&spoken))
			if !errors.Is(err, voice.ErrUnreachable) || len(spoken) != 1 || spoken[0].text != cannotReachDispatch {
				t.Errorf("Answer = %v, spoken %+v; want only %q and voice.ErrUnreachable", err, spoken, cannotReachDispatch)
			}
			if searched := len(r.records.searches()); searched != 0 {
				t.Errorf("searched %d times, want none", searched)
			}
		})
	}
	t.Run("the list known, Dispatch does not answer the reads", func(t *testing.T) {
		r := newRig(t)
		r.records.mu.Lock()
		r.records.err = fmt.Errorf("dispatch: %w", dispatch.ErrUnreachable)
		r.records.mu.Unlock()
		var spoken []said
		err := r.concierge.Answer(context.Background(), &r.exchange, "4592", recorder(&spoken))
		if !errors.Is(err, voice.ErrUnreachable) || len(spoken) != 1 || spoken[0].text != cannotReachDispatch {
			t.Errorf("Answer = %v, spoken %+v; want only %q and voice.ErrUnreachable", err, spoken, cannotReachDispatch)
		}
	})
	t.Run("control: a cached list's refresh failed, no project has the number", func(t *testing.T) {
		// Dispatch answered the reads that it has no such issue: that is no
		// outage, though the list's last refresh failed.
		r := newRig(t)
		projectsRefreshFailed.set(t, r)
		var spoken []said
		err := r.concierge.Answer(context.Background(), &r.exchange, "4592", recorder(&spoken))
		if errors.Is(err, voice.ErrUnreachable) || len(spoken) != 1 || spoken[0].text != noIssueFound {
			t.Errorf("Answer = %v, spoken %+v; want only %q and no outage", err, spoken, noIssueFound)
		}
	})
	t.Run("control: the list known, no project has the number", func(t *testing.T) {
		r := newRig(t)
		var spoken []said
		err := r.concierge.Answer(context.Background(), &r.exchange, "4592", recorder(&spoken))
		if errors.Is(err, voice.ErrUnreachable) || len(spoken) != 1 || spoken[0].text != noIssueFound {
			t.Errorf("Answer = %v, spoken %+v; want only %q and no outage", err, spoken, noIssueFound)
		}
	})
}

// TestEverySearchAQuestionCausesIsCounted (R6): every search a question
// causes counts against its one limit, maxSearches: the read-ahead's while
// it is asked, the answer's read and the model's together. A turn that names
// a key with no issue searches its other words; on main each interim of it
// searched again, uncounted, and with the model's search one status question
// reached Dispatch's full search five times.
func TestEverySearchAQuestionCausesIsCounted(t *testing.T) {
	r := newRig(t)
	r.concierge.searchSettle = 20 * time.Millisecond
	heldIssue(r, "session", false)
	r.records.hits = []dispatch.SearchHit{{Kind: "issue", IssueKey: "ABC-12", Title: "Realtime voice", Score: 0.9}}
	r.records.kwHits = slices.Clone(r.records.hits)
	searchCall := func(id string) *genai.GenerateContentResponse {
		return chunk(call(id, "search_issues", map[string]any{"query": "realtime voice budget plan"}, ""))
	}
	r.model.turns = [][]*genai.GenerateContentResponse{{searchCall("s1")}, {searchCall("s2")}, {chunk(text("ABC-12: PR 80 is in review."))}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	heard := []string{
		"what is happening with ABC-99 and the realtime",
		"what is happening with ABC-99 and the realtime voice",
		"what is happening with ABC-99 and the realtime voice budget",
		"what is happening with ABC-99 and the realtime voice budget plan",
	}
	for _, words := range heard {
		r.concierge.Heard(ctx, &r.exchange, words)
		// The read this interim started, or the one before when it started
		// none: a key read alone, or the turn's search once the key named
		// none (#192).
		eventually(t, "the read-ahead's read of "+words+" ended", func() bool {
			r.concierge.mu.Lock()
			defer r.concierge.mu.Unlock()
			p := r.concierge.prefetchLocked(&r.exchange)
			if p == nil || p.reads[p.current] == nil {
				return false
			}
			return ended(p.reads[p.current])
		})
	}
	var spoken []said
	err := r.concierge.Answer(ctx, &r.exchange, heard[len(heard)-1], recorder(&spoken))
	if err != nil || len(spoken) == 0 {
		t.Fatalf("Answer = %v, spoken %+v; want an answer", err, spoken)
	}
	if full := r.records.searches(); len(full) > maxSearches {
		t.Errorf("the question reached the full search %d times (%q), want at most %d", len(full), full, maxSearches)
	}
}

// TestAKeySaidAloneAfterDidYouMeanAnswersTheFirstQuestion (Main on #191): a
// key said alone after "Did you mean …?", one not offered included, answers
// the question first asked, about that key, as a key said for "What is its
// key?" does: a bare key is no conversation. A key inside other words stays
// side talk.
func TestAKeySaidAloneAfterDidYouMeanAnswersTheFirstQuestion(t *testing.T) {
	for _, tc := range []struct {
		reply string
		key   bool
	}{
		{"ABC-14", true},
		{"it's ABC-14", true},
		{"ABC-14 rollout", false},
		{"ABC-14, the first one", false},
	} {
		t.Run(tc.reply, func(t *testing.T) {
			r := newRig(t)
			heldIssue(r, "session", false)
			r.records.mu.Lock()
			r.records.issues["ABC-14"] = claimed("ABC-14", "session", "agent-9", "Other")
			r.records.mu.Unlock()
			r.agents.states["agent-9"] = envoy.SessionInfo{SessionID: "agent-9", State: envoy.StateLive}
			r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("It is open."))}}
			r.concierge.offerPick(&r.exchange, twoOfferedTurn, twoOffered)
			var spoken []said
			err := r.concierge.Answer(context.Background(), &r.exchange, tc.reply, recorder(&spoken))
			r.agents.mu.Lock()
			sent := slices.Clone(r.agents.sent)
			r.agents.mu.Unlock()
			if !tc.key {
				if !errors.Is(err, voice.ErrSideTalk) || len(spoken) != 0 || len(r.records.calls()) != 0 || len(sent) != 0 {
					t.Errorf("Answer = %v, spoken %+v, read %q, sent %+v; want side talk", err, spoken, r.records.calls(), sent)
				}
				return
			}
			if err != nil || r.exchange.IssueKey != "ABC-14" || len(sent) != 1 || sent[0].Target != "agent-9" || !strings.Contains(sent[0].Message, "Is the tap merged?") {
				t.Errorf("Answer = %v, took %q, spoken %+v, sent %+v; want the question first asked sent to ABC-14's holder", err, r.exchange.IssueKey, spoken, sent)
			}
		})
	}
}

// TestAQuestionTheModelAsksIsAnsweredLikeWisprsOwn (Main on #191): the
// Supervisor takes the next utterance after any spoken line that ends in "?"
// with no question mark of its own, whoever wrote the line; so a question
// the model asks is kept as an offer too. After it, side talk is not
// answered, a key said alone answers the question first asked, and a
// question is answered.
func TestAQuestionTheModelAsksIsAnsweredLikeWisprsOwn(t *testing.T) {
	const asked = "what is happening with the realtime voice"
	for _, tc := range []struct{ reply, want string }{
		{"okay, let's move on to the budget", "side talk"},
		{"ABC-12", "the key"},
		{"what about the wake word", "answered"},
	} {
		t.Run(tc.reply, func(t *testing.T) {
			r := newRig(t)
			heldIssue(r, "session", false)
			r.records.hits = []dispatch.SearchHit{{Kind: "issue", IssueKey: "ABC-12", Title: "Realtime voice", Score: 0.9}}
			r.records.kwHits = slices.Clone(r.records.hits)
			r.model.turns = [][]*genai.GenerateContentResponse{
				{chunk(text("ABC-12 is in progress. Do you mean its status or its holder?"))},
				{chunk(text("ABC-12 is in progress."))},
			}
			if spoken := answerTurn(t, r, asked); len(spoken) == 0 || !strings.HasSuffix(spoken[len(spoken)-1].text, "?") {
				t.Fatalf("spoken %+v, want the model's question last", spoken)
			}
			searchedBefore := len(r.records.searches())
			var spoken []said
			err := r.concierge.Answer(context.Background(), &r.exchange, tc.reply, recorder(&spoken))
			searched := len(r.records.searches()) - searchedBefore
			switch tc.want {
			case "side talk":
				if !errors.Is(err, voice.ErrSideTalk) || len(spoken) != 0 || searched != 0 {
					t.Errorf("Answer = %v, spoken %+v, searched %d; want side talk", err, spoken, searched)
				}
			case "the key":
				if prompt := lastPrompt(r); err != nil || r.exchange.IssueKey != "ABC-12" || !strings.Contains(prompt, "said after the wake word: "+asked) {
					t.Errorf("Answer = %v, took %q, prompt:\n%s\nwant the question first asked answered about ABC-12", err, r.exchange.IssueKey, prompt)
				}
			default:
				if err != nil || len(spoken) == 0 || searched != 1 {
					t.Errorf("Answer = %v, spoken %+v, searched %d; want the question answered", err, spoken, searched)
				}
			}
		})
	}
}

// TestALateReadAheadReadsNothingForSideTalk (Main on #191): the Supervisor
// hands a turn to the Concierge (Answer) and tells the read-ahead (Heard) of
// its words apart, so Heard can come after the answer took the reply. For
// side talk it must read nothing then either: no issue, no holder and no
// search, before the exchange's end cancels it.
func TestALateReadAheadReadsNothingForSideTalk(t *testing.T) {
	const reply = "okay, on to ABC-99 and the rollout then"
	r := newRig(t)
	r.concierge.searchSettle = 20 * time.Millisecond
	heldIssue(r, "session", false)
	r.records.hits = []dispatch.SearchHit{{Kind: "issue", IssueKey: "ABC-12", Title: "Realtime voice", Score: 0.9}}
	r.records.kwHits = slices.Clone(r.records.hits)
	r.concierge.offerPick(&r.exchange, twoOfferedTurn, twoOffered)
	ctx := context.Background() // the exchange has not ended yet
	if err := r.concierge.Answer(ctx, &r.exchange, reply, recorder(new([]said))); !errors.Is(err, voice.ErrSideTalk) {
		t.Fatalf("Answer = %v, want voice.ErrSideTalk", err)
	}
	r.concierge.Heard(ctx, &r.exchange, reply)
	time.Sleep(200 * time.Millisecond)
	r.records.mu.Lock()
	searched := len(r.records.queries) + len(r.records.kwQueries)
	r.records.mu.Unlock()
	r.agents.mu.Lock()
	lookups := r.agents.lookups
	r.agents.mu.Unlock()
	if calls := r.records.calls(); len(calls) != 0 || searched != 0 || lookups != 0 {
		t.Errorf("the late read-ahead read %q, searched %d times, looked up %d holders; want nothing", calls, searched, lookups)
	}
}

// TestAnUnreadSpelledKeyInSideTalkSaysNothing (Sol's review of #191 at
// 1cbcb494): with an offer pending and the project list unable to tell,
// side talk that holds letters which could be a key is still side talk: the
// voice says nothing and plays no error sound. A key said alone, a question
// and a pick in the letters still get "I cannot reach Dispatch yet.".
func TestAnUnreadSpelledKeyInSideTalkSaysNothing(t *testing.T) {
	for _, tc := range []struct {
		reply  string
		outage bool
	}{
		{"okay, on to A B C 12 and the rollout then", false},
		{"I O U twenty dollars", false},
		{"A B C twelve", true},
		{"it's A B C twelve", true},
		{"what about A B C twelve", true},
		{"A B C thirteen, the second one", true},
	} {
		t.Run(tc.reply, func(t *testing.T) {
			r := newRig(t)
			projectsReadFails.set(t, r)
			r.concierge.offerPick(&r.exchange, twoOfferedTurn, twoOffered)
			var spoken []said
			err := r.concierge.Answer(context.Background(), &r.exchange, tc.reply, recorder(&spoken))
			if tc.outage {
				if !errors.Is(err, voice.ErrUnreachable) || len(spoken) != 1 || spoken[0].text != cannotReachDispatch {
					t.Errorf("Answer = %v, spoken %+v; want only %q and voice.ErrUnreachable", err, spoken, cannotReachDispatch)
				}
				return
			}
			if !errors.Is(err, voice.ErrSideTalk) || len(spoken) != 0 {
				t.Errorf("Answer = %v, spoken %+v; want nothing said and voice.ErrSideTalk", err, spoken)
			}
		})
	}
}

// TestALateReadAheadDoesNotSearchForTheTurnAnswered (Sol's review of #191 at
// 1cbcb494): the Supervisor starts an answer and tells the read-ahead of the
// same turn apart, so the read-ahead can come once the answer took its
// count. It must then do nothing: one question makes at most maxSearches
// searches, the answer's and the model's here.
func TestALateReadAheadDoesNotSearchForTheTurnAnswered(t *testing.T) {
	const turn = "what is happening with realtime voice budget"
	r := newRig(t)
	r.concierge.searchSettle = time.Millisecond
	heldIssue(r, "session", false)
	r.records.hits = []dispatch.SearchHit{{Kind: "issue", IssueKey: "ABC-12", Title: "Realtime voice budget", Score: 0.9}}
	r.records.kwHits = slices.Clone(r.records.hits)
	r.model.turns = [][]*genai.GenerateContentResponse{
		{chunk(call("s1", "search_issues", map[string]any{"query": "voice budget"}, ""))},
		{chunk(text("ABC-12 is in progress."))},
	}
	r.model.onCall = func(n int) {
		if n == 0 {
			r.concierge.Heard(context.Background(), &r.exchange, turn)
			time.Sleep(100 * time.Millisecond) // past the read-ahead's settle
		}
	}
	answerTurn(t, r, turn)
	if searches := r.records.searches(); len(searches) != maxSearches || searches[1] != "voice budget" {
		t.Errorf("searches %q, want the answer's and the model's, %d in all", searches, maxSearches)
	}
}

// TestALateReadAheadSpendsNoLaterQuestionsSearches: a read-ahead of a turn
// whose answer already ended (the Supervisor told it late) reads nothing,
// so the next question keeps its searches: its own read and the model's.
func TestALateReadAheadSpendsNoLaterQuestionsSearches(t *testing.T) {
	const first, second = "what is happening with the realtime voice", "what about the wake word"
	r := newRig(t)
	r.concierge.searchSettle = time.Millisecond
	voiceIssue(r)
	r.model.turns = [][]*genai.GenerateContentResponse{
		{chunk(text("ABC-12 is in progress."))},
		{chunk(call("s1", "search_issues", map[string]any{"query": "wake word"}, ""))},
		{chunk(text("ABC-12 is in progress."))},
	}
	answerTurn(t, r, first)
	r.concierge.Heard(context.Background(), &r.exchange, first)
	time.Sleep(100 * time.Millisecond) // past the read-ahead's settle
	if searches := r.records.searches(); len(searches) != 1 {
		t.Fatalf("searches %q after the first answer and its late read-ahead, want the answer's alone", searches)
	}
	r.exchange.Turn++ // the next turn, as the Supervisor numbers it
	answerTurn(t, r, second)
	if searches := r.records.searches()[1:]; !slices.Equal(searches, []string{second, "wake word"}) {
		t.Errorf("the second question searched %q, want its own read and the model's", searches)
	}
}

// TestAReadAheadTheAnswerDidNotBeginSearchesNothing: a read-ahead of the
// turn begun before its answer, for words the answer does not take (it
// searches the final words itself), searches nothing once the answer began,
// so the model keeps the question's second search.
func TestAReadAheadTheAnswerDidNotBeginSearchesNothing(t *testing.T) {
	r := newRig(t)
	r.concierge.searchSettle = 100 * time.Millisecond
	heldIssue(r, "session", false)
	r.records.hits = []dispatch.SearchHit{{Kind: "issue", IssueKey: "ABC-12", Title: "Realtime voice budget", Score: 0.9}}
	r.records.searchDelay = 300 * time.Millisecond // the answer's own search runs past the read-ahead's settle
	r.model.turns = [][]*genai.GenerateContentResponse{
		{chunk(call("s1", "search_issues", map[string]any{"query": "voice budget"}, ""))},
		{chunk(text("ABC-12 is in progress."))},
	}
	// A status question with a fresh record asks no holder, so the model
	// keeps its tools.
	r.concierge.Heard(context.Background(), &r.exchange, "what is happening with the realtime voice")
	answerTurn(t, r, "what is happening with the realtime voice budget")
	if searches := r.records.searches(); !slices.Equal(searches, []string{"what is happening with the realtime voice budget", "voice budget"}) {
		t.Errorf("searches %q, want the answer's and the model's", searches)
	}
}

// TestANumberWithNoDispatchClientIsAnOutage (Sol's review of #191 at
// 1cbcb494): with no Dispatch client (no token or address, a supported
// state), a number said alone cannot be looked up: the room hears that
// Dispatch is out of reach, with the error sound, as it does for a key.
func TestANumberWithNoDispatchClientIsAnOutage(t *testing.T) {
	for _, turn := range []string{"ABC-12", "4592", "ask him for 4592"} {
		t.Run(turn, func(t *testing.T) {
			r := newRig(t)
			c, err := New(&Config{Generator: r.model, Store: r.store, Now: func() time.Time { return testNow }})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(c.Stop)
			var spoken []said
			err = c.Answer(context.Background(), &r.exchange, turn, recorder(&spoken))
			if !errors.Is(err, voice.ErrUnreachable) || len(spoken) != 1 || spoken[0].text != cannotReachDispatch {
				t.Errorf("Answer = %v, spoken %+v; want only %q and voice.ErrUnreachable", err, spoken, cannotReachDispatch)
			}
		})
	}
}
