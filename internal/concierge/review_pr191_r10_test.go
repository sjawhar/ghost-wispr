package concierge

import (
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/dispatch"
)

// Sol's tenth review of #191 at a4f63fc
// (https://github.com/sjawhar/ghost-wispr/pull/191#pullrequestreview-6065885261),
// from Sol's probes, through the real Supervisor and Concierge.

// answers is n model turns that each say ABC-12 is in progress.
func answers(n int) [][]*genai.GenerateContentResponse {
	out := make([][]*genai.GenerateContentResponse, n)
	for i := range out {
		out[i] = []*genai.GenerateContentResponse{chunk(text("ABC-12 is in progress."))}
	}
	return out
}

// hearNow has the room say words in one final, each word heard now.
func (r *cutRig) hearNow(words string, endpoint bool) {
	at := make([]time.Time, len(strings.Fields(words)))
	now := time.Now()
	for i := range at {
		at[i] = now
	}
	r.hearEnding(words, true, endpoint, at)
}

// TestAVendorsNewQuestionIsNotCutByTheTurnBefore (Sol's tenth review of
// #191, P2 1): a driving voice can ask its next question before the room's
// next words reach the unit. At a4f63fc the turn before was still the
// exchange's AskerTurn and Finals, and its take-back cut the new question:
// "what is the status of ABC-42?" was answered as the ABC-12 question the
// first turn kept. The Supervisor clears a handed turn, and the answer cuts
// only the turn the finals are of. Control: the turn before with no
// take-back.
func TestAVendorsNewQuestionIsNotCutByTheTurnBefore(t *testing.T) {
	for _, opening := range []string{
		"Wispr, what about ABC-99? never mind. what is the status of ABC-12?",
		"Wispr, what is the status of ABC-12?",
	} {
		t.Run(opening, func(t *testing.T) {
			r := newCutRigOpening(t, []string{opening}, false,
				[]*genai.GenerateContentResponse{chunk(text("ABC-12 is in progress."))},
				[]*genai.GenerateContentResponse{chunk(text("ABC-42 is in progress."))},
			)
			r.records.mu.Lock()
			r.records.issues["ABC-42"] = &dispatch.Issue{Key: "ABC-42", Title: "A new vendor question", Status: "in_progress", UpdatedAt: testNow}
			r.records.mu.Unlock()
			if first := r.answer(t, "first", ""); first.IssueKey != "ABC-12" || first.Say == "" {
				t.Fatalf("control: the first answer took %q, said %q; want ABC-12", first.IssueKey, first.Say)
			}
			r.speak(t)
			if next := r.answer(t, "next", "what is the status of ABC-42?"); next.IssueKey != "ABC-42" {
				t.Errorf("the vendor's new question took %q, said %q; want ABC-42", next.IssueKey, next.Say)
			}
		})
	}
}

// TestCancelledWordsLeaveTheRoomsRecentWords (Sol's tenth review of #191,
// P2 2): words a take-back cancelled reach no model call, in their turn or a
// later one. At a4f63fc only the prompt of a turn whose own finals held the
// take-back left them out: with an endpoint between "Wispr, what about
// ABC-42? never mind" and "what about ABC-12?", or for the next question
// after the answer, the model read ABC-42 in the room's recent words. The
// Supervisor now drops them from the room's recent words it keeps. Control:
// the first answer of the turn with no endpoint.
func TestCancelledWordsLeaveTheRoomsRecentWords(t *testing.T) {
	for _, tc := range []struct {
		name      string
		endpoints bool
		next      bool // a next question after the answer is played
	}{
		{"control: one turn", false, false},
		{"an endpoint between the take-back and the question", true, false},
		{"the next turn's question", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newCutRigOpening(t, []string{"Wispr, what about ABC-42? never mind", "what about ABC-12?"}, tc.endpoints, answers(3)...)
			if first := r.answer(t, "first", "what about ABC-12?"); first.Say == "" {
				t.Fatal("control: the question was not answered")
			}
			if prompt := lastPrompt(r.rig); strings.Contains(prompt, "ABC-42") {
				t.Errorf("the model was given the cancelled words:\n%s", prompt)
			}
			if !tc.next {
				return
			}
			r.speak(t)
			r.hearNow("Wispr, what is the status of ABC-12?", true)
			if second := r.answer(t, "next", "what is the status of ABC-12?"); second.Say == "" {
				t.Fatal("control: the next question was not answered")
			}
			if prompt := lastPrompt(r.rig); strings.Contains(prompt, "ABC-42") {
				t.Errorf("the next turn's model was given the cancelled words:\n%s", prompt)
			}
		})
	}
}

// TestAQuotedWordIsNoTakeBack (Sol's tenth review of #191, P2 3): a quoted
// word is what the room talks about, never a take-back. At a4f63fc the
// quotes went before the take-back check, and `did ABC-12, "Cancel", ship?`
// sent the holder "Ship?". Controls: a question, "don't cancel the rollout?",
// a real take-back, and a turn taken back whole.
func TestAQuotedWordIsNoTakeBack(t *testing.T) {
	for _, tc := range []struct {
		name, words, want string
		silent            bool
	}{
		{"control: a question", "Wispr, what is happening with ABC-12?", "what is happening with ABC-12?", false},
		{"control: cancel inside a clause", "Wispr, what about ABC-12, don't cancel the rollout?", "what about ABC-12, don't cancel the rollout?", false},
		{"control: a take-back, then a question", "Wispr, what about ABC-42? never mind. what about ABC-12?", "what about ABC-12?", false},
		{"control: the turn taken back whole", "Wispr, what about ABC-42? never mind", "", true},
		{"a quoted title", "Wispr, did ABC-12, \"Cancel\", ship?", "did ABC-12, \"Cancel\", ship?", false},
		{"a quoted phrase", "Wispr, what about ABC-12, \"never mind\", in the rollout?", "what about ABC-12, \"never mind\", in the rollout?", false},
		{"a curly-quoted title", "Wispr, did ABC-12, “Stop”, ship?", "did ABC-12, “Stop”, ship?", false},
		{"a single-quoted title", "Wispr, did ABC-12, ‘Stop’, ship?", "did ABC-12, ‘Stop’, ship?", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newCutRigOpening(t, []string{tc.words}, false, answers(2)...)
			r.records.mu.Lock()
			r.records.issues["ABC-42"] = &dispatch.Issue{Key: "ABC-42", Title: "A cancelled issue", Status: "todo", UpdatedAt: testNow}
			r.records.mu.Unlock()
			out := r.answer(t, "clause", strings.TrimPrefix(tc.words, "Wispr, "))
			prompt := lastPrompt(r.rig)
			r.agents.mu.Lock()
			messages := slices.Clone(r.agents.sent)
			r.agents.mu.Unlock()
			if tc.silent {
				if out.Say != "" || len(messages) != 0 || prompt != "" {
					t.Errorf("said %q, sent %d, prompted %q; want nothing", out.Say, len(messages), prompt)
				}
				return
			}
			if out.Say == "" {
				t.Fatal("the question was not answered")
			}
			if !strings.Contains(prompt, "said after the wake word: "+tc.want+"\n") {
				t.Errorf("the model was given:\n%s\nwant the question %q", prompt, tc.want)
			}
			for _, m := range messages {
				if !strings.Contains(strings.ToLower(m.Message), strings.ToLower(tc.want)) {
					t.Errorf("the holder was sent %q; want %q", m.Message, tc.want)
				}
			}
		})
	}
}

// TestATurnTakenBackWholeDropsItsWaitingSearch (Sol's tenth review of #191,
// P2 4): the read-ahead of "what is happening with the realtime voice
// budget?" waits for its words to settle before it searches. "never mind",
// a final of the same turn, takes it back whole. At a4f63fc the read-ahead
// then read nothing new and left the waiting read alone: Dispatch got the
// cancelled question's search. Control: with no take-back the read searches
// once.
func TestATurnTakenBackWholeDropsItsWaitingSearch(t *testing.T) {
	for _, takenBack := range []bool{true, false} {
		name := "control: no take-back"
		if takenBack {
			name = "never mind"
		}
		t.Run(name, func(t *testing.T) {
			r := newCutRigOpening(t, []string{"Wispr."}, false, answers(2)...)
			r.concierge.mu.Lock()
			r.concierge.searchSettle = 300 * time.Millisecond
			r.concierge.mu.Unlock()
			r.hearNow("what is happening with the realtime voice budget?", false)
			r.seen.mu.Lock()
			x := r.seen.heard[len(r.seen.heard)-1]
			r.seen.mu.Unlock()
			r.concierge.mu.Lock()
			var pending *read
			if p := r.concierge.prefetchLocked(&x); p != nil {
				pending = p.reads[p.current]
			}
			r.concierge.mu.Unlock()
			if pending == nil {
				t.Fatal("control: no read ahead waits")
			}
			if searches := r.records.searches(); len(searches) != 0 {
				t.Fatalf("control: searched %q before the take-back", searches)
			}
			if takenBack {
				r.hearNow("never mind", true)
			}
			select {
			case <-pending.done: // a cancelled read closes done too
			case <-time.After(r7Deadline):
				t.Fatal("the waiting read neither searched nor was cancelled")
			}
			searches := r.records.searches()
			switch {
			case takenBack && len(searches) != 0:
				t.Errorf("the question taken back was searched: %q", searches)
			case !takenBack && len(searches) != 1:
				t.Errorf("control: searched %q, want the question once", searches)
			}
		})
	}
}
