package concierge

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/voice"
	"github.com/sjawhar/ghost-wispr/internal/voice/dgagent"
	"github.com/sjawhar/ghost-wispr/internal/voice/gptlive"
)

// Sol's sixteenth review of #191 at 1432f2f
// (https://github.com/sjawhar/ghost-wispr/pull/191#pullrequestreview-6069899310),
// from Sol's probes, through the real Supervisor, Concierge and vendor
// clients, with the R15 loopback sockets.

// delegate has GPT-Live make id's delegation and returns the question its
// model received and how many holder messages exist after it.
func delegate(t *testing.T, w *vendorWire, v *watchedVoice, r *cutRig, id string, offset int) (string, int) {
	t.Helper()
	w.send(map[string]any{"type": "session.delegation.created", "event_id": "delegate-" + id, "offset_ms": offset, "delegation": map[string]any{"id": id, "type": "delegation", "target": "client"}})
	within(t, v.calls, "delegation "+id)
	within(t, v.results, "answer to "+id)
	return modelQuestion(r), len(holderMessages(r))
}

// TestVoiceBsCurrentQuestionWinsOverOldVendorWords (Sol's sixteenth review
// #191, P2 1): GPT-Live heard ABC-42 before the room took it back. Its first
// delegation correctly used Room's "what about ABC-12?", then the old
// lastHeard won when the second delegation heard nothing new and brought
// ABC-42 back to the model and holder. The first delegation uses the room's
// request; the second, with no new final of the room's, has no request and
// asks nothing at all (Main, after Sol's nineteenth review of #191), so the
// old vendor words can win nothing. Control: no take-back keeps ABC-42.
func TestVoiceBsCurrentQuestionWinsOverOldVendorWords(t *testing.T) {
	for _, cut := range []bool{true, false} {
		t.Run(fmt.Sprintf("take-back %v", cut), func(t *testing.T) {
			url, conns := loopbackVendor(t, false)
			live, err := gptlive.New(gptlive.Config{URL: url, Token: func(context.Context) (string, error) { return "loopback-not-a-secret", nil }})
			if err != nil {
				t.Fatal(err)
			}
			v := watch(live)
			r := openingRig(t, v)
			heldIssue(r.rig, "session", false)
			r.model.turns = answers(4)
			r.hearNow("Wispr, what about ABC-42?", false)
			w := within(t, conns, "vendor connection")
			w.next(t, "session.start")
			within(t, v.opened, "opened voice")
			if cut {
				r.hearNow("never mind. what about ABC-12?", false)
			} else {
				r.hearNow("what about ABC-12?", false)
			}
			// GPT-Live heard only the question it has now lost or had replaced.
			w.send(map[string]any{"type": "session.input_transcript.delta", "event_id": "old", "delta": "what about ABC-42?", "start_ms": 0, "end_ms": 100})
			question, _ := delegate(t, w, v, r, "first", 1000)
			if contains := strings.Contains(question, "ABC-42"); contains != !cut {
				t.Errorf("delegation first asked %q; ABC-42 present %v, want %v", question, contains, !cut)
			}
			sendDelegation(w, "second", 2000)
			if q := toolWithin(t, v, 600*time.Millisecond); q != "" {
				t.Errorf("delegation second asked %q with no new final of the room's", q)
			}
		})
	}
}

// TestVoiceBsNewLocalQuestionIsNotHiddenByOldVendorWords (Sol's sixteenth
// review #191, P2 1): after a delegation that heard a new question, the room
// takes it back and asks a different one while GPT-Live hears no new words.
// At 1432f2f lastHeard restored "when will ABC-12 ship?" and dedupe hid the
// new "is ABC-12 merged?" question. Control: when GPT-Live hears the new
// question it is sent too.
func TestVoiceBsNewLocalQuestionIsNotHiddenByOldVendorWords(t *testing.T) {
	for _, hearsReplacement := range []bool{false, true} {
		name := "the vendor hears no replacement"
		if hearsReplacement {
			name = "control: the vendor hears the replacement"
		}
		t.Run(name, func(t *testing.T) {
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
			r.hearNow("never mind. when will ABC-12 ship?", false)
			w.send(map[string]any{"type": "session.input_transcript.delta", "event_id": "shipping", "delta": "when will ABC-12 ship?", "start_ms": 0, "end_ms": 100})
			delegate(t, w, v, r, "shipping", 1000)
			before := len(holderMessages(r))
			r.hearNow("never mind. is ABC-12 merged?", false)
			if hearsReplacement {
				w.send(map[string]any{"type": "session.input_transcript.delta", "event_id": "merged", "delta": "is ABC-12 merged?", "start_ms": 200, "end_ms": 300})
			}
			question, after := delegate(t, w, v, r, "merged", 2000)
			if question != "is ABC-12 merged?" {
				t.Errorf("the new local question was hidden: got %q, want is ABC-12 merged?", question)
			}
			if after <= before {
				t.Errorf("the new local question was deduplicated: before=%d after=%d", before, after)
			}
		})
	}
}

// heldOpen starts the real vendor client, then holds just Open's return: the
// same race as a worker started before it published its Session to Supervisor.
type heldOpen struct {
	inner   voice.Voice
	started chan voice.Exchange
	release chan struct{}
}

func (*heldOpen) DrivesTurn() bool { return true }

func (v *heldOpen) Open(ctx context.Context, x *voice.Exchange) (voice.Session, error) {
	s, err := v.inner.Open(ctx, x)
	if err == nil {
		v.started <- *x
		<-v.release
	}
	return s, err
}

// TestRoomIsEmptyAfterItsExchangeEnded (Sol's sixteenth review #191, P2 2):
// a capture-gate close can end an exchange while a real vendor's Open is
// suspended after its workers start. At 1432f2f Room then returned the old
// question and the late Deepgram asker timer injected it after the gate
// closed. Room returns no words for an ended or canceled exchange. Control:
// a live exchange still sends its question.
func TestRoomIsEmptyAfterItsExchangeEnded(t *testing.T) {
	for _, ended := range []bool{false, true} {
		name := "control: active"
		if ended {
			name = "capture gate closed before Open returns"
		}
		t.Run(name, func(t *testing.T) {
			url, conns := loopbackVendor(t, true)
			agent, err := dgagent.New(&dgagent.Config{APIKey: "loopback-not-a-secret", URL: url})
			if err != nil {
				t.Fatal(err)
			}
			hold := &heldOpen{inner: agent, started: make(chan voice.Exchange, 1), release: make(chan struct{})}
			r := openingRig(t, hold)
			defer close(hold.release)
			r.hearNow("Wispr, what about ABC-42?", false)
			w := within(t, conns, "vendor connection")
			w.next(t, "Settings")
			x := within(t, hold.started, "client started before Open returns")
			if ended {
				r.sup.GateClosed(1)
			}
			room := x.Room()
			if ended && (room.Asker != "" || len(room.Recent) != 0) {
				t.Errorf("ended exchange exposes words through Room: %q / %+v", room.Asker, room.Recent)
			}
			timer := time.NewTimer(7 * time.Second)
			defer timer.Stop()
			for {
				select {
				case b := <-w.frames:
					var m struct {
						Type    string `json:"type"`
						Content string `json:"content"`
					}
					_ = json.Unmarshal(b, &m)
					if m.Type != "InjectUserMessage" {
						continue
					}
					if ended {
						t.Errorf("private turn sent after gate closed: %s", b)
					} else if !strings.Contains(m.Content, "ABC-42") {
						t.Errorf("control lost live question: %s", b)
					}
					return
				case <-timer.C:
					if !ended {
						t.Fatal("control: active exchange sent no delayed question")
					}
					return
				}
			}
		})
	}
}
