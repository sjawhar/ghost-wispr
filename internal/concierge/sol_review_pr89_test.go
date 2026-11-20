package concierge

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/envoy"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

func TestSolFastAnswerKeepsNamedKey(t *testing.T) {
	for _, c := range []struct {
		name, reply, want string
		turns             int
	}{
		{"named-key control", "ABC-12 is in progress, updated 2 hours ago.", "ABC-12 is in progress, updated 2 hours ago.", 1},
		{"two keyless replies", "It is in progress, updated 2 hours ago.", "ABC-12: " + fixedSentence, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			defer r.concierge.Stop()
			heldIssue(r, "session", false)
			for range c.turns {
				r.model.turns = append(r.model.turns, []*genai.GenerateContentResponse{chunk(text(c.reply))})
			}
			got := answerTurn(t, r, "what is happening with ABC-12?")
			var all []string
			for _, s := range got {
				all = append(all, s.text)
			}
			spoken := strings.Join(all, " ")
			t.Logf("voice text: %s", spoken)
			if spoken != c.want {
				t.Fatalf("voice text = %q, want %q", spoken, c.want)
			}
			if len(r.model.requests) != c.turns {
				t.Fatalf("model calls = %d, want %d", len(r.model.requests), c.turns)
			}
			if c.name == "named-key control" && !strings.Contains(r.model.requests[0][0].Parts[0].Text, "Begin your first sentence with ABC-12") {
				t.Fatal("the model prompt does not require the named key first")
			}
		})
	}
}

func TestSolNamedKeyAllowsLaterSentencesWithoutTheKey(t *testing.T) {
	r := newRig(t)
	heldIssue(r, "session", false)
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("ABC-12 is in review. PR 80 is in review."))}}
	got := answerTurn(t, r, "what is happening with ABC-12")
	if text := []string{got[0].text, got[1].text}; !slices.Equal(text, []string{"ABC-12 is in review.", "PR 80 is in review."}) {
		t.Fatalf("spoken = %q, want both sentences", text)
	}
	if len(r.model.requests) != 1 {
		t.Fatalf("model calls = %d, want 1", len(r.model.requests))
	}
}

func TestSolExplicitAskReachesUnresponsiveHolder(t *testing.T) {
	for _, state := range []envoy.SessionState{envoy.StateLive, envoy.StateUnresponsive} {
		t.Run(string(state), func(t *testing.T) {
			r := newRig(t)
			defer r.concierge.Stop()
			heldIssue(r, "session", false)
			r.agents.states["agent-7"] = envoy.SessionInfo{SessionID: "agent-7", State: state}
			r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("ABC-12 is in progress, updated 2 hours ago."))}}
			got := answerTurn(t, r, "ask the agent on ABC-12 when it will be ready?")
			r.agents.mu.Lock()
			sent := slices.Clone(r.agents.sent)
			r.agents.mu.Unlock()
			t.Logf("spoken=%+v sent=%d", got, len(sent))
			if len(sent) != 1 || sent[0].Target != "agent-7" {
				t.Fatalf("sent = %+v, want one question to the known holder", sent)
			}
			task, err := r.store.VoiceTask("evt-1")
			if err != nil || task.SessionID != "meeting-1" || task.IssueKey != "ABC-12" || task.Holder != "session:agent-7" || task.State != storage.VoiceTaskWorking {
				t.Fatalf("task = %+v, %v; want the working task for the meeting and holder", task, err)
			}
			if state == envoy.StateUnresponsive {
				if !slices.ContainsFunc(got, func(s said) bool { return s.text == "I asked the agent on ABC-12. The message waits until it runs." }) {
					t.Fatalf("spoken = %+v, want the queued-message line", got)
				}
			}
		})
	}
}

// TestSolExplicitAskKeepsKeyWhenSendOutlastsAnswer: a promised asked line
// must not stand in for a key it fails to say before the answer ends.
func TestSolExplicitAskKeepsKeyWhenSendOutlastsAnswer(t *testing.T) {
	for _, slow := range []bool{false, true} {
		name := "control_send_answers"
		if slow {
			name = "send_outlasts_fast_answer"
		}
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			heldIssue(r, "session", false)
			r.concierge.fastBound = 300 * time.Millisecond
			r.concierge.knownBound = time.Second
			release := make(chan struct{})
			finished := make(chan struct{})
			r.agents.onSend = func(context.Context, string) error {
				defer close(finished)
				if slow {
					<-release
				}
				return nil
			}
			r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("It is in progress, updated 2 hours ago."))}}
			var got []said
			err := r.concierge.Answer(context.Background(), &r.exchange, "ask the agent on ABC-12 when it will be ready?", recorder(&got))
			close(release)
			select {
			case <-finished:
			case <-time.After(time.Second):
				t.Fatal("the probe never reached the send")
			}
			var texts []string
			for _, s := range got {
				texts = append(texts, s.text)
			}
			spoken := strings.Join(texts, " ")
			if err != nil {
				t.Fatalf("Answer = %v, spoken %q", err, spoken)
			}
			if !strings.Contains(spoken, "ABC-12") {
				t.Fatalf("spoken = %q, want the explicitly requested key", spoken)
			}
			if !strings.Contains(spoken, "I asked the agent on ABC-12.") {
				t.Fatalf("spoken = %q, want the asked line", spoken)
			}
		})
	}
}

func TestSolPendingAskRetriesBeforeFixedSentence(t *testing.T) {
	for _, keyless := range []bool{false, true} {
		name := "control_keyed_first_attempt"
		if keyless {
			name = "keyless_first_then_valid_second"
		}
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			heldIssue(r, "session", false)
			r.concierge.fastBound = time.Second
			r.concierge.knownBound = 2 * time.Second
			release, finished := make(chan struct{}), make(chan struct{})
			r.agents.onSend = func(context.Context, string) error {
				defer close(finished)
				<-release
				return nil
			}
			first := "ABC-12 is in progress, updated 2 hours ago."
			if keyless {
				first = "It is in progress, updated 2 hours ago."
			}
			r.model.turns = [][]*genai.GenerateContentResponse{
				{chunk(text(first))},
				{chunk(text("ABC-12 is in progress, updated 2 hours ago."))},
			}
			var got []said
			err := r.concierge.Answer(context.Background(), &r.exchange, "ask the agent on ABC-12 when it will be ready?", recorder(&got))
			close(release)
			select {
			case <-finished:
			case <-time.After(time.Second):
				t.Fatal("the probe did not reach the held send")
			}
			var texts []string
			for _, s := range got {
				texts = append(texts, s.text)
			}
			spoken := strings.Join(texts, " ")
			wantCalls := 1
			if keyless {
				wantCalls = 2
			}
			if err != nil {
				t.Fatalf("Answer = %v, model calls = %d, spoken %q", err, len(r.model.requests), spoken)
			}
			if len(r.model.requests) != wantCalls {
				t.Fatalf("model calls = %d, want %d before a fixed fallback", len(r.model.requests), wantCalls)
			}
			if strings.Contains(spoken, fixedSentence) {
				t.Fatalf("the fixed fallback replaced an available valid answer: %q", spoken)
			}
		})
	}
}
