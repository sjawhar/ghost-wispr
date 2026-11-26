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

// TestADelegationCancelsItsAcknowledgementAtOnce guards the real-vendor run of
// 2026-11-26: a withdrawn delegation reached its silent ending roomWait later,
// and in that gap GPT-Live spoke its automatic acknowledgement ("I'll check on
// that.") into the room. The empty thinking append that cancels it must go out
// as the delegation arrives, well before the question is resolved, whether the
// room has an asker or not.
func TestADelegationCancelsItsAcknowledgementAtOnce(t *testing.T) {
	for _, asker := range []string{"", "what about ABC-42?"} {
		name := "no asker, the question is still being waited for"
		if asker != "" {
			name = "control: the room has an asker"
		}
		t.Run(name, func(t *testing.T) {
			tools := newFakeTools(voice.AnswerResult{Say: "x"})
			x := testExchange(newFakeOutput(), tools)
			x.Room = func() voice.RoomView {
				return voice.RoomView{Live: true, Asker: asker, Revision: 1}
			}
			srv := newLiveServer(t)
			_, c, _ := openSession(t, srv, x)

			begin := time.Now()
			c.delegate("item_ack")
			first := c.expect(evThinkingAppend)
			took := time.Since(begin)
			if first.DelegationID == nil || *first.DelegationID != "item_ack" || first.Content != "" {
				t.Fatalf("first frame for the delegation %+v, want an empty thinking append", first)
			}
			if took >= roomWait/2 {
				t.Errorf("the acknowledgement was cancelled %v after the delegation, want well inside roomWait (%v): the vendor speaks in that gap", took, roomWait)
			}
		})
	}
}

// TestADelegationsAcknowledgementReachesNoSpeaker guards the office Pi run of
// 2026-11-26 at 66f73dc: GPT-Live speaks an acknowledgement of its own
// ("Let me check that.", 498 ms after the delegation reached the client) and
// no instruction stops it. Its audio reaches the room only through us, so the
// client holds the speaker from the delegation's arrival until it resolves.
// A withdrawn delegation passes on nothing at all; an answered one passes on
// the answer.
func TestADelegationsAcknowledgementReachesNoSpeaker(t *testing.T) {
	for _, answered := range []bool{false, true} {
		name := "withdrawn: the room hears nothing"
		if answered {
			name = "control: the answer still plays"
		}
		t.Run(name, func(t *testing.T) {
			out := newFakeOutput()
			tools := newFakeTools(voice.AnswerResult{Say: "ABC-42 is in review."})
			x := testExchange(out, tools)
			asker := ""
			if answered {
				asker = "what about ABC-42?"
			}
			x.Room = func() voice.RoomView {
				return voice.RoomView{Live: true, Asker: asker, Revision: 1}
			}
			srv := newLiveServer(t)
			s, c, _ := openSession(t, srv, x)

			c.delegate("item_hold")
			// The acknowledgement GPT-Live speaks after delegating.
			c.audioDelta(ramp(1000, SampleRate/2))
			if !answered {
				if first := c.expect(evThinkingAppend); first.Content != "" {
					t.Fatalf("silent completion content %q", first.Content)
				}
				// Nothing GPT-Live sent while the delegation was outstanding
				// may reach the speaker.
				time.Sleep(200 * time.Millisecond)
				if n := len(out.samples()); n != 0 {
					t.Errorf("the room heard %d samples of the acknowledgement, want none", n)
				}
				return
			}
			waitEvent(t, s, voice.EventToolCall)
			tools.call(t)
			if n := len(out.samples()); n != 0 {
				t.Errorf("the room heard %d samples of the acknowledgement, want none", n)
			}
			// The answer, a speech of its own once the hold is released,
			// plays.
			time.Sleep(speechGap + 100*time.Millisecond)
			c.audioDelta(ramp(5000, 2400))
			out.nextSpeech(t).waitSamples(t, 2400)
		})
	}
}
