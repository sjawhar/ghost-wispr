package gptlive

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// Sol's eighteenth review of #191 at 377b117
// (https://github.com/sjawhar/ghost-wispr/pull/191#pullrequestreview-6073778863),
// gptlive-package tests with a mock Room, where the room's view is exact.

// TestADelegationWithOnlyHistoryStaysSilent (finding 2): the room has recent
// meeting speech but no current asker. There is no history fallback; the
// delegation completes silently rather than turning an unrelated sentence
// into a question. At 377b117 the model received the unrelated line.
func TestADelegationWithOnlyHistoryStaysSilent(t *testing.T) {
	tools := newFakeTools(voice.AnswerResult{Say: "x"})
	x := testExchange(newFakeOutput(), tools)
	x.Room = func() voice.RoomView {
		return voice.RoomView{Live: true, Recent: []voice.TurnText{{Text: "The unrelated meeting starts after lunch."}}, Revision: 2}
	}
	srv := newLiveServer(t)
	_, c, _ := openSession(t, srv, x)

	c.delegate("item_h")
	if first := c.expect(evThinkingAppend); first.DelegationID == nil || *first.DelegationID != "item_h" || first.Content != "" {
		t.Fatalf("silent completion %+v, want an empty thinking append for item_h", first)
	}
	select {
	case call := <-tools.calls:
		t.Fatalf("a delegation with only history ran the tool: %s", call.ID)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestADelegationOnALiveOpeningRunsTheTool (finding 4): a live room before its
// first final has revision zero, which is a normal opening, not the ended
// view. A delegation then answers the opening question. At 377b117 revision
// zero was taken for the ended view and the delegation completed silently.
// Control: the ended view (Live false) completes silently.
func TestADelegationOnALiveOpeningRunsTheTool(t *testing.T) {
	for _, live := range []bool{true, false} {
		name := "ended view"
		if live {
			name = "live opening, revision zero"
		}
		t.Run(name, func(t *testing.T) {
			tools := newFakeTools(voice.AnswerResult{Say: "x"})
			x := testExchange(newFakeOutput(), tools)
			x.Room = func() voice.RoomView {
				return voice.RoomView{Live: live, Asker: "what about ABC-42?", Revision: 0}
			}
			srv := newLiveServer(t)
			s, c, _ := openSession(t, srv, x)

			c.delegate("item_o")
			if !live {
				if first := c.expect(evThinkingAppend); first.Content != "" {
					t.Fatalf("ended view ran content %q, want a silent completion", first.Content)
				}
				select {
				case call := <-tools.calls:
					t.Fatalf("the ended view ran the tool: %s", call.ID)
				case <-time.After(100 * time.Millisecond):
				}
				return
			}
			ev := waitEvent(t, s, voice.EventToolCall)
			var args voice.AnswerArgs
			if err := json.Unmarshal(ev.Call.Args, &args); err != nil {
				t.Fatal(err)
			}
			if args.Question != "what about ABC-42?" {
				t.Errorf("live opening question %q, want the room's asker", args.Question)
			}
		})
	}
}
