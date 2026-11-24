package concierge

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/dispatch"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

func probe106Issue(r *rig) {
	r.records.issues["INFRA-4592"] = &dispatch.Issue{Key: "INFRA-4592", Title: "Realtime voice", Status: "in_progress", UpdatedAt: testNow}
}

func TestReview106OrdinaryNumbersNeedExistingIssue(t *testing.T) {
	for _, turn := range []string{
		"what happened at 1500?", "what happened at 15:00?", "what about 1500 dollars?",
		"what happened in 2026?", "what about room 1500?", "what about phone 415-555-0199?",
	} {
		t.Run(turn, func(t *testing.T) {
			r := newRig(t)
			probe106Issue(r)
			r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("The record does not say."))}}
			spoken := answerTurn(t, r, turn)
			if slices.ContainsFunc(spoken, func(s said) bool { return strings.HasPrefix(s.text, "Did you mean") }) {
				t.Fatalf("no matching number exists: spoken=%+v", spoken)
			}
			if len(r.agents.sent) != 0 {
				t.Fatal("ordinary number sent a question")
			}
		})
	}
	r := newRig(t)
	probe106Issue(r)
	spoken := answerTurn(t, r, "ask him for 4592")
	if len(spoken) != 1 || spoken[0].text != "Did you mean Infra forty-five ninety-two?" {
		t.Fatalf("positive control=%+v", spoken)
	}
}

func TestReview106NumberLookupPayload(t *testing.T) {
	// A number-only turn sends no search query. A turn with other words
	// runs the keyword search (Main's round-2 design).
	t.Run("number only", func(t *testing.T) {
		r := newRig(t)
		probe106Issue(r)
		_ = answerTurn(t, r, "ask him for 4592")
		r.records.mu.Lock()
		queries := append(slices.Clone(r.records.queries), r.records.kwQueries...)
		r.records.mu.Unlock()
		if len(queries) != 0 {
			t.Errorf("number-only turn searched: %q", queries)
		}
	})
	t.Run("candidate key reads", func(t *testing.T) {
		r := newRig(t)
		probe106Issue(r)
		_ = answerTurn(t, r, "ask him for 4592")
		r.records.mu.Lock()
		calls := slices.Clone(r.records.issueCalls)
		r.records.mu.Unlock()
		for _, call := range calls {
			if !strings.HasSuffix(call, "-4592") {
				t.Errorf("not a key candidate: %q", call)
			}
		}
	})
}

func TestReview106SingleConfirmationReplies(t *testing.T) {
	for _, tc := range []struct {
		reply, wantKey string
		wantOK         bool
	}{
		{"yes", "INFRA-4592", true}, {"Yes.", "INFRA-4592", true}, {"no", "", true}, {"No.", "", true},
		{"let us discuss the budget", "", false}, {"what does yes mean?", "", false},
		{"yes and no", "", false}, {"the first item in our budget is rent", "", false},
	} {
		t.Run(tc.reply, func(t *testing.T) {
			r := newRig(t)
			r.concierge.offerPick(&r.exchange, "ask him for 4592", []candidate{{"INFRA-4592", "Realtime voice"}})
			_, key, ok := r.concierge.takePick(&r.exchange, tc.reply)
			if key != tc.wantKey || ok != tc.wantOK {
				t.Errorf("takePick=%q,%v want=%q,%v", key, ok, tc.wantKey, tc.wantOK)
			}
			if _, _, again := r.concierge.takePick(&r.exchange, "yes"); again {
				t.Error("offer survived next turn")
			}
		})
	}
}

func TestReview106R5AndBareNumber(t *testing.T) {
	for _, clear := range []bool{true, false} {
		t.Run(map[bool]string{true: "clear description", false: "ambiguous description"}[clear], func(t *testing.T) {
			r := newRig(t)
			unclearSearch(r)
			probe106Issue(r)
			if clear {
				r.records.hits[0].Score = 0.012
			}
			r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("ABC-12 is in progress."))}}
			spoken := answerTurn(t, r, "what is happening with the issue about realtime voice in room 4592?")
			want := "Did you mean A B C twelve, or A B C thirteen?"
			if clear {
				want = "ABC-12 is in progress."
			}
			if len(spoken) == 0 || spoken[0].text != want {
				t.Errorf("spoken=%+v want first=%q", spoken, want)
			}
		})
	}
}

type review106SlowRecords struct{ Records }

func (r review106SlowRecords) Issue(ctx context.Context, key string) (*dispatch.Issue, error) {
	if key == "XYZ-4592" {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return r.Records.Issue(ctx, key)
}

func TestReview106KnownCandidateDoesNotWaitForUnrelatedProject(t *testing.T) {
	r := newRig(t)
	probe106Issue(r)
	r.concierge.records = review106SlowRecords{Records: r.records}
	var spoken []said
	err := r.concierge.Answer(context.Background(), &r.exchange, "ask him for 4592", func(_ context.Context, text string, mode voice.SayMode) error {
		spoken = append(spoken, said{text, mode})
		return nil
	})
	if err != nil {
		t.Errorf("known candidate was lost: %v", err)
	}
	if !slices.ContainsFunc(spoken, func(s said) bool { return s.text == "Did you mean Infra forty-five ninety-two?" }) {
		t.Error("known matching issue never confirmed")
	}
}

func TestReview106SideTalkDoesNotAskTheHolder(t *testing.T) {
	r := newR7Rig(t)
	records := r.c.records.(*fakeRecords)
	records.mu.Lock()
	records.issues["ABC-4592"] = &dispatch.Issue{
		Key: "ABC-4592", Title: "Realtime voice", Status: "in_progress",
		UpdatedAt: time.Now(),
		Claim:     &dispatch.Claim{Actor: actor("session", "agent-7", "Worker")},
	}
	records.mu.Unlock()
	r.hear("Wispr, ask him for 4592")
	r7Until(t, "confirmation spoken", func() bool {
		return slices.ContainsFunc(r.voice.said(), func(s r7Said) bool { return s.text == "Did you mean A B C forty-five ninety-two?" })
	})
	time.Sleep(20 * time.Millisecond)
	r.hear("the first item in our budget is rent")
	r7Until(t, "side-talk turn finished", func() bool {
		opened, closed := r.voice.counts()
		return opened == 1 && closed == 1
	})
	r.agents.mu.Lock()
	sent := slices.Clone(r.agents.sent)
	r.agents.mu.Unlock()
	if len(sent) != 0 {
		t.Errorf("side talk selected the unconfirmed issue and sent %d question(s) to %q", len(sent), sent[0].Target)
	}
}
