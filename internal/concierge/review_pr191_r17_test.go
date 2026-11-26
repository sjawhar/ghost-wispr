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

// TestVoiceBsFreshFollowupSurvivesAnUnrelatedFinal (Sol's seventeenth
// review #191, P2 1): a final such as "okay, sure", with no take-back or
// replacement question, advances Room's revision but must not discard a
// fresh vendor follow-up. At a8f712e the model got "what about ABC-42? okay,
// sure". Controls: no final, and a repeated original question final.
func TestVoiceBsFreshFollowupSurvivesAnUnrelatedFinal(t *testing.T) {
	for _, final := range []string{"", "okay, sure", "what about ABC-42?"} {
		t.Run(final, func(t *testing.T) {
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
			if final != "" {
				r.hearNow(final, false)
			}
			fresh := "when will ABC-12 ship?"
			w.send(map[string]any{"type": "session.input_transcript.delta", "event_id": "fresh", "delta": fresh, "start_ms": 0, "end_ms": 100})
			question, _ := delegate(t, w, v, r, "fresh", 2000)
			if question != fresh || modelQuestion(r) != fresh {
				t.Errorf("fresh vendor question lost after %q: tool %q, model %q; want %q", final, question, modelQuestion(r), fresh)
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
				if got := completion(t, w, id); got != "The question was withdrawn. Do not say anything." {
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
