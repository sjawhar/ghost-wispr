package concierge

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/dispatch"
)

func TestBareNumberConfirmation(t *testing.T) {
	t.Run("ask him for 4592", func(t *testing.T) {
		r := newRig(t)
		r.records.issues["INFRA-4592"] = &dispatch.Issue{Key: "INFRA-4592", Title: "Realtime voice", Status: "in_progress", UpdatedAt: testNow}
		spoken := answerTurn(t, r, "ask him for 4592")
		if len(spoken) != 1 || spoken[0].text != "Did you mean Infra forty-five ninety-two?" {
			t.Fatalf("spoken = %+v, want the confirmation", spoken)
		}
		if len(r.agents.sent) != 0 {
			t.Fatalf("sent = %+v, want no question before confirmation", r.agents.sent)
		}

		r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("INFRA-4592 is in progress."))}}
		confirmed := answerTurn(t, r, "yes")
		if len(confirmed) == 0 || confirmed[0].text != "INFRA-4592 is in progress." {
			t.Fatalf("yes = %+v, want the confirmed issue's fact", confirmed)
		}
	})

	t.Run("ask him for 1500 dollars", func(t *testing.T) {
		r := newRig(t)
		spoken := answerTurn(t, r, "ask him for 1500 dollars")
		if strings.Contains(strings.Join(textsOf(spoken), " "), "Did you mean") {
			t.Fatalf("spoken = %+v, want no confirmation without a matching issue", spoken)
		}
		if len(r.agents.sent) != 0 {
			t.Fatalf("sent = %+v, want no question", r.agents.sent)
		}
	})

	t.Run("the most recently active issue is first", func(t *testing.T) {
		r := newRig(t)
		r.records.issues["ABC-4592"] = &dispatch.Issue{Key: "ABC-4592", Title: "Older work", Status: "in_progress", UpdatedAt: testNow.Add(-time.Hour)}
		r.records.issues["INFRA-4592"] = &dispatch.Issue{Key: "INFRA-4592", Title: "Newer work", Status: "in_progress", UpdatedAt: testNow.Add(-time.Minute)}
		spoken := answerTurn(t, r, "ask him for 4592")
		want := "Did you mean Infra forty-five ninety-two, or A B C forty-five ninety-two?"
		if len(spoken) != 1 || spoken[0].text != want {
			t.Fatalf("spoken = %+v, want %q", spoken, want)
		}
	})
}

func textsOf(spoken []said) []string {
	texts := make([]string, len(spoken))
	for i := range spoken {
		texts[i] = spoken[i].text
	}
	return texts
}

func TestBareNumberConfirmationDoesNotMakeAHimForKey(t *testing.T) {
	r := newRig(t)
	r.records.issues["INFRA-4592"] = &dispatch.Issue{Key: "INFRA-4592", UpdatedAt: testNow}
	if target := r.concierge.readTarget(context.Background(), "ask him for 4592", true); strings.HasPrefix(target, "keys:") {
		t.Fatalf("readTarget = %q, want a non-key target", target)
	}
}
