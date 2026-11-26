package concierge

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/voice"
	"github.com/sjawhar/ghost-wispr/internal/voice/gptlive"
)

// Sol's eighteenth review of #191 at 377b117
// (https://github.com/sjawhar/ghost-wispr/pull/191#pullrequestreview-6073778863),
// through the real Supervisor, Concierge and GPT-Live client. The room is the
// only source of a delegation's question text (Main).

// openVoiceB opens the real GPT-Live client over a loopback socket on the
// real Supervisor and Concierge, with the room already asking ABC-42, and
// returns the rig, the watched voice and the vendor wire.
func openVoiceB(t *testing.T) (*cutRig, *watchedVoice, *vendorWire) {
	t.Helper()
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
	return r, v, w
}

// sendDelegation makes GPT-Live raise one client delegation.
func sendDelegation(w *vendorWire, id string, offset int) {
	w.send(map[string]any{"type": "session.delegation.created", "event_id": "delegate-" + id, "offset_ms": offset, "delegation": map[string]any{"id": id, "type": "delegation", "target": "client"}})
}

// toolWithin returns the next tool call's question within d, or "" if none
// ran (the delegation completed silently).
func toolWithin(t *testing.T, v *watchedVoice, d time.Duration) string {
	t.Helper()
	select {
	case c := <-v.calls:
		<-v.results
		var args voice.AnswerArgs
		if err := json.Unmarshal(c.Args, &args); err != nil {
			t.Fatalf("tool args %s: %v", c.Args, err)
		}
		return args.Question
	case <-time.After(d):
		return ""
	}
}

// TestVoiceBsRetryAfterWholeTakebackNeverRestoresTheQuestion (finding 1): a
// whole-turn take-back empties the room's asker; later delegations with no new
// room question never resurrect the canceled one. At 377b117 a second retry's
// cached vendor words restored ABC-42. Control: with no take-back the room's
// asker is answered.
func TestVoiceBsRetryAfterWholeTakebackNeverRestoresTheQuestion(t *testing.T) {
	for _, cut := range []bool{true, false} {
		name := "control: no take-back"
		if cut {
			name = "whole turn taken back"
		}
		t.Run(name, func(t *testing.T) {
			r, v, w := openVoiceB(t)
			if q, _ := delegate(t, w, v, r, "d1", 1000); q != "what about ABC-42?" {
				t.Fatalf("first question %q, want the room's asker", q)
			}
			if cut {
				r.hearNow("never mind", false)
			}
			for i, id := range []string{"d2", "d3"} {
				// GPT-Live re-hears the canceled question; its transcript is
				// never the question, so no retry resurrects it.
				w.send(map[string]any{"type": "session.input_transcript.delta", "event_id": "heard-" + id, "delta": "what about ABC-42?", "start_ms": 0, "end_ms": 100})
				sendDelegation(w, id, 2000+i*1000)
				q := toolWithin(t, v, 2500*time.Millisecond)
				if cut && strings.Contains(q, "ABC-42") {
					t.Errorf("retry %s restored the canceled question: %q", id, q)
				}
				if !cut && q != "what about ABC-42?" {
					t.Errorf("control retry %s %q, want the room's asker", id, q)
				}
			}
		})
	}
}

// TestVoiceBsBoundedWaitPicksUpARoomFollowup (finding 3): a delegation that
// arrives while the room's asker is empty waits, bounded, for the room's next
// local final and answers it. At 377b117 it completed silently at once. The
// genuine follow-up is said by the room, never GPT-Live's transcript.
func TestVoiceBsBoundedWaitPicksUpARoomFollowup(t *testing.T) {
	r, v, w := openVoiceB(t)
	if q, _ := delegate(t, w, v, r, "d1", 1000); q != "what about ABC-42?" {
		t.Fatalf("first question %q", q)
	}
	r.hearNow("never mind", false)
	// The delegation arrives with no room asker; the room finalizes the
	// follow-up a moment later.
	sendDelegation(w, "d2", 2000)
	time.Sleep(150 * time.Millisecond)
	r.hearNow("Wispr, when will ABC-12 ship?", false)
	if q := toolWithin(t, v, 2500*time.Millisecond); q != "when will ABC-12 ship?" {
		t.Errorf("the awaited room follow-up reached the tool as %q, want \"when will ABC-12 ship?\"", q)
	}
}
