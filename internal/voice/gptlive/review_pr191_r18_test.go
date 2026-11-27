package gptlive

import (
	"encoding/json"
	"sync"
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
				return voice.RoomView{Live: live, Asker: "what about ABC-42?", Request: "what about ABC-42?", Revision: 0}
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
// 2026-11-26: nothing was sent on a delegation until its silent ending, so
// GPT-Live's automatic acknowledgement had the whole gap to play. The empty
// thinking append that cancels it, the documented best effort, goes out as
// the delegation arrives, whether the room has a request yet or not.
func TestADelegationCancelsItsAcknowledgementAtOnce(t *testing.T) {
	for _, asker := range []string{"", "what about ABC-42?"} {
		name := "no request yet, the delegation waits for its final"
		if asker != "" {
			name = "control: the room has a request"
		}
		t.Run(name, func(t *testing.T) {
			tools := newFakeTools(voice.AnswerResult{Say: "x"})
			x := testExchange(newFakeOutput(), tools)
			x.Room = func() voice.RoomView {
				return voice.RoomView{Live: true, Asker: asker, Request: asker, Revision: 1}
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
			if took >= 250*time.Millisecond {
				t.Errorf("the acknowledgement was cancelled %v after the delegation, want at once", took)
			}
		})
	}
}

// TestARealAnswerPlaysInFull guards Sol's nineteenth review of #191, findings
// 4 and 5: a speaker hold muted every sample of a real answer that followed
// the acknowledgement with less than 400 ms between them, the final having
// come while the acknowledgement played, and one withdrawn delegation's hold
// discarded another delegation's whole answer. GPT-Live's audio names no
// response, so the client holds nothing back: the answer plays sample for
// sample, with no gap after the acknowledgement, when the room's final comes
// during the acknowledgement, and while another delegation is outstanding.
func TestARealAnswerPlaysInFull(t *testing.T) {
	for _, tc := range []struct {
		name        string
		other, late bool
	}{
		{"right after the acknowledgement, with no gap", false, false},
		{"the room's final comes during the acknowledgement", false, true},
		{"while another delegation is outstanding", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := newFakeOutput()
			tools := newFakeTools(voice.AnswerResult{Say: "ABC-42 is in review."})
			x := testExchange(out, tools)
			request := "what about ABC-42?"
			if tc.late {
				request = "" // the delegation waits for the room's final
			}
			rs := withRoom(x, request)
			var claimed sync.Once
			x.Claim = func(uint64, uint64) bool {
				won := false
				claimed.Do(func() { won = true })
				return won // one request: a second delegation stays outstanding
			}
			srv := newLiveServer(t)
			s, c, _ := openSession(t, srv, x)

			if tc.other {
				c.delegate("item_other")
			}
			c.delegate("item_answer")
			ack, answer := ramp(1000, 2400), ramp(5000, 2400)
			c.audioDelta(ack) // the acknowledgement GPT-Live speaks after delegating
			if tc.late {
				// The acknowledgement has begun to play when the room's final
				// gives the waiting delegation its request.
				out.nextSpeech(t).waitSamples(t, 1)
				rs.set(voice.RoomView{Live: true, Asker: "what about ABC-42?", Request: "what about ABC-42?", Revision: 2})
			}
			waitEvent(t, s, voice.EventToolCall)
			tools.call(t)
			c.audioDelta(answer) // the answer, straight after, with no gap
			want := append(append([]int16(nil), ack...), answer...)
			// Every speech's samples, in order: a mute or a hold that split the
			// stream cannot hide a loss in a speech this test did not open.
			var got []int16
			for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
				if got = out.samples(); len(got) >= len(want) {
					break
				}
			}
			if len(got) != len(want) {
				t.Fatalf("the speaker got %d samples, want the acknowledgement and the whole answer, %d", len(got), len(want))
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("sample %d is %d, want %d: the answer did not play in full and in order", i, got[i], want[i])
				}
			}
		})
	}
}

// TestAWithdrawnDelegationsAcknowledgementStillPlays states the limit Main
// accepted after Sol's nineteenth review of #191 (findings 4 to 6): GPT-Live
// speaks an acknowledgement of its own after it delegates, whatever it is
// told, and its audio carries no response or item id and has no completion
// event, so no client rule can tell it from an answer. After a whole-turn
// take-back the room may therefore hear that acknowledgement ("Checking."),
// which holds no words of the room, but never the withdrawn question or an
// answer to it: no tool runs, and the delegation is closed thinking-only,
// with no commentary and no session-wide instruction that could silence
// another delegation's answer.
func TestAWithdrawnDelegationsAcknowledgementStillPlays(t *testing.T) {
	out := newFakeOutput()
	tools := newFakeTools(voice.AnswerResult{Say: "x"})
	x := testExchange(out, tools)
	rs := withRoom(x, "")
	srv := newLiveServer(t)
	_, c, _ := openSession(t, srv, x)

	c.delegate("item_withdrawn")
	if first := c.expect(evThinkingAppend); first.DelegationID == nil || *first.DelegationID != "item_withdrawn" || first.Content != "" {
		t.Fatalf("first frame %+v, want the acknowledgement cancelled", first)
	}
	ack := ramp(1000, 2400)
	c.audioDelta(ack)
	// The room's next final takes the request back: it leaves nothing to ask.
	rs.set(voice.RoomView{Live: true, Revision: 2})
	closing := c.expect(evThinkingAppend)
	if closing.DelegationID == nil || *closing.DelegationID != "item_withdrawn" || closing.Content != "" {
		t.Fatalf("completion %+v, want a thinking-only close of item_withdrawn", closing)
	}
	out.nextSpeech(t).waitSamples(t, len(ack))
	deadline := time.After(200 * time.Millisecond)
	for {
		select {
		case ev := <-c.in:
			if ev.Type == evCommentaryAppend || ev.Type == evInstructionsAppend {
				t.Fatalf("a withdrawn delegation sent %s, want nothing past its thinking-only close", ev.raw)
			}
			continue
		case call := <-tools.calls:
			t.Fatalf("a withdrawn delegation ran the tool: %s", call.ID)
		case <-deadline:
		}
		break
	}
}
