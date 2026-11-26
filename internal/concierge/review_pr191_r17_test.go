package concierge

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/voice/gptlive"
)

// Sol's seventeenth review of #191 at a8f712e
// (https://github.com/sjawhar/ghost-wispr/pull/191#pullrequestreview-6071577133),
// from Sol's probes, through the real Supervisor, Concierge and GPT-Live
// client, with the R15 loopback socket.

// A follow-up the room says, as a local final, is the next delegation's
// question; GPT-Live's own transcript is never the question (Main, after
// Sol's eighteenth review of #191). A driving voice keeps one turn for the
// whole exchange, so the room's asker grows; the delegation asks what the
// room has said since the last one, never the question already answered.
// Controls: no intervening final, an unrelated one, and the room repeating
// the original question, each of which the room genuinely said.
func TestVoiceBsRoomFollowupReachesTheTool(t *testing.T) {
	for _, tc := range []struct{ final, want string }{
		{"", "when will ABC-12 ship?"},
		{"okay, sure", "okay, sure when will ABC-12 ship?"},
		{"what about ABC-42?", "what about ABC-42? when will ABC-12 ship?"},
	} {
		t.Run(tc.final, func(t *testing.T) {
			url, conns := loopbackVendor(t, false)
			live, err := gptlive.New(gptlive.Config{URL: url, Token: func(context.Context) (string, error) { return "loopback-not-a-secret", nil }})
			if err != nil {
				t.Fatal(err)
			}
			v := watch(live)
			r := openingRig(t, v)
			heldIssue(r.rig, "session", false)
			r.model.turns = answers(5)
			r.hearNow("Wispr, what about ABC-42?", false)
			w := within(t, conns, "vendor connection")
			w.next(t, "session.start")
			within(t, v.opened, "opened voice")
			delegate(t, w, v, r, "first", 1000)
			if tc.final != "" {
				r.hearNow(tc.final, false)
			}
			// GPT-Live's own transcript of a follow-up is ignored; only the
			// room's local final gives the next question.
			w.send(map[string]any{"type": "session.input_transcript.delta", "event_id": "heard", "delta": "when will ABC-12 ship?", "start_ms": 0, "end_ms": 100})
			r.hearNow("Wispr, when will ABC-12 ship?", false)
			question, _ := delegate(t, w, v, r, "fresh", 2000)
			if question != tc.want || modelQuestion(r) != tc.want {
				t.Errorf("room follow-up after %q: tool %q, model %q; want %q", tc.final, question, modelQuestion(r), tc.want)
			}
		})
	}
}

// completion waits for the thinking-only function result that closes id.
func completion(t *testing.T, w *vendorWire, id string) string {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case raw := <-w.frames:
			var m struct {
				Type         string `json:"type"`
				DelegationID string `json:"delegation_id"`
				Content      string `json:"content"`
			}
			if json.Unmarshal(raw, &m) == nil && m.DelegationID == id {
				if m.Type != "session.thinking.append" {
					t.Errorf("completion type %q, want thinking only: %s", m.Type, raw)
				}
				return m.Content
			}
		case <-timer.C:
			t.Fatalf("no completion for delegation %s", id)
			return ""
		}
	}
}

// TestVoiceBsWholeTakebackCompletesSilently (Sol's seventeenth review #191,
// P2 2 and 3): a whole-turn take-back invalidates vendor words heard before
// it. At a8f712e GPT-Live restored ABC-42; after the branch suppressed that
// it left the delegation waiting. A live Room with no asker and no usable
// vendor words now completes in thinking only: no Concierge tool call and no
// spoken line. Control: the uncanceled question calls the tool.
func TestVoiceBsWholeTakebackCompletesSilently(t *testing.T) {
	for _, cut := range []bool{false, true} {
		name := "control: no take-back"
		if cut {
			name = "whole turn taken back"
		}
		t.Run(name, func(t *testing.T) {
			url, conns := loopbackVendor(t, false)
			live, err := gptlive.New(gptlive.Config{URL: url, Token: func(context.Context) (string, error) { return "loopback-not-a-secret", nil }})
			if err != nil {
				t.Fatal(err)
			}
			v := watch(live)
			r := openingRig(t, v)
			r.model.turns = answers(2)
			r.hearNow("Wispr, what about ABC-42?", false)
			w := within(t, conns, "vendor connection")
			w.next(t, "session.start")
			within(t, v.opened, "opened voice")
			if cut {
				r.hearNow("never mind", false)
			}
			w.send(map[string]any{"type": "session.input_transcript.delta", "event_id": "old", "delta": "what about ABC-42?", "start_ms": 0, "end_ms": 100})
			id := "whole-takeback"
			w.send(map[string]any{"type": "session.delegation.created", "event_id": "delegate", "offset_ms": 1000, "delegation": map[string]any{"id": id, "type": "delegation", "target": "client"}})
			if cut {
				if got := completion(t, w, id); got != "" {
					t.Errorf("silent completion %q", got)
				}
				if len(v.calls) != 0 {
					t.Errorf("whole take-back ran %d tool calls", len(v.calls))
				}
				return
			}
			within(t, v.calls, "answer tool")
			within(t, v.results, "answer result")
		})
	}
}
