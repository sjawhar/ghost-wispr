package gptlive

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/voice"
)

type solR5CurrentQuestionTools struct{}

func (solR5CurrentQuestionTools) Run(_ context.Context, call voice.ToolCall) (json.RawMessage, error) {
	var args voice.AnswerArgs
	if err := json.Unmarshal(call.Args, &args); err != nil {
		return nil, err
	}
	key := "ABC-123"
	if strings.Contains(args.Question, "XYZ-456") {
		key = "XYZ-456"
	}
	return json.Marshal(voice.AnswerResult{Say: key + " is in review.", IssueKey: key})
}

func TestSolR5AuditUsesTheCurrentFollowUpKey(t *testing.T) {
	for _, tc := range []struct {
		name, spoken string
		bad          bool
	}{
		{"correct current key", "XYZ-456 is in review.", false},
		{"wrong key from initial question", "ABC-123 is in review.", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := testExchange(newFakeOutput(), solR5CurrentQuestionTools{})
			rs := withRoom(x, "What is happening with ABC-123?")
			srv := newLiveServer(t)
			s, c, _ := openSession(t, srv, x)
			c.heard("What is happening with ABC-123?")
			c.delegate("initial")
			first := c.expect(evCommentaryAppend)
			if first.Content != "ABC-123 is in review." {
				t.Fatalf("initial backend result %q", first.Content)
			}
			c.speak(first.Content)
			if n := waitFactCheck(t, s); n != 0 {
				t.Fatalf("initial unchanged key control failures=%d", n)
			}
			rs.set(voice.RoomView{Live: true, Asker: "What is happening with XYZ-456?", Revision: 2})
			c.heard("What is happening with XYZ-456?")
			c.delegate("followup")
			next := c.expect(evCommentaryAppend)
			if next.Content != "XYZ-456 is in review." {
				t.Fatalf("followup backend result %q, want XYZ-456", next.Content)
			}
			c.speak(tc.spoken)
			n := waitFactCheck(t, s)
			if (n > 0) != tc.bad {
				t.Errorf("current question is XYZ-456, spoken=%q, failures=%d, expected invalid=%t", tc.spoken, n, tc.bad)
			}
		})
	}
}

func TestSolR5HeldAnswerRetainsExchangeKey(t *testing.T) {
	x := testExchange(newFakeOutput(), newFakeTools(voice.AnswerResult{}))
	// Supervisor.speakLocked supplies the canonical pending-answer key even
	// though a held exchange has no AskerTurn or Recent turn.
	x.AskerTurn, x.Recent, x.IssueKey = "", nil, "ABC-123"
	srv := newLiveServer(t)
	s, c, _ := openSession(t, srv, x)
	done := make(chan error, 1)
	go func() { done <- s.Say(context.Background(), "ABC-123 is in review.", voice.Answer) }()
	c.expect(evCommentaryAppend)
	c.speak("It is in review.")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := waitFactCheck(t, s); n == 0 {
		t.Fatal("held answer omitted its canonical key but reported zero failures")
	}
}
