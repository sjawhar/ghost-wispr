package concierge

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/storeguard"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// stopWhileSendHeld stops r's Concierge while an ask's send is held, releases
// the send held after held, and fails if Stop returned before the release, or
// if anything wrote the store once Stop had returned, when the unit closes
// the store (storeguard records it). started starts the call that sends;
// sending is closed once its send is held.
func stopWhileSendHeld(t *testing.T, r *rig, started func(), sending <-chan struct{}, release chan struct{}, finished <-chan struct{}) {
	t.Helper()
	guard := storeguard.New(t, r.store)
	started()
	select {
	case <-sending:
	case <-time.After(5 * time.Second):
		t.Fatal("control: the ask never reached its send")
	}
	const held = 200 * time.Millisecond
	released := make(chan time.Time, 1)
	time.AfterFunc(held, func() {
		released <- time.Now()
		close(release)
	})
	r.concierge.Stop()
	stopped := time.Now()
	guard.Close(t)
	select {
	case at := <-released:
		if stopped.Before(at) {
			t.Errorf("Stop returned %v before the held send was released: it does not wait for the ask", at.Sub(stopped))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the send was never released")
	}
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("the call that asked did not end")
	}
	time.Sleep(100 * time.Millisecond) // a write the stop left would land now
	guard.Check(t)
}

// TestStopWaitsForEveryAskThatWritesTheStore fails if Stop returns while an
// ask still runs that writes the ledger once its send returns (it keys the
// task by the send's event): the tool call of a vendor voice (B and C call
// Tools(x).Run on goroutines of their own), and the ask an answer of voice A
// leaves running when its record took the answer's whole time.
func TestStopWaitsForEveryAskThatWritesTheStore(t *testing.T) {
	t.Run("a vendor voice's tool call", func(t *testing.T) {
		r := newRig(t)
		r.name(t, "agent-7")
		sending, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
		r.agents.onSend = func(context.Context, string) error {
			close(sending)
			<-release
			return nil
		}
		stopWhileSendHeld(t, r, func() {
			go func() {
				defer close(finished)
				raw, _ := json.Marshal(map[string]string{"session": "agent-7", "question": "is it merged"})
				_, _ = r.concierge.Tools(&r.exchange).Run(context.Background(), voice.ToolCall{ID: "call-1", Name: "ask_agent", Args: raw})
			}()
		}, sending, release, finished)
	})
	t.Run("an ask the answer leaves running", func(t *testing.T) {
		r := newRig(t)
		heldIssue(r, "session", false)
		r.concierge.fastBound = 300 * time.Millisecond
		r.concierge.knownBound = time.Second
		sending, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
		r.agents.onSend = func(context.Context, string) error {
			close(sending)
			<-release
			return nil
		}
		r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("ABC-12 is in progress, updated 2 hours ago."))}}
		answered := make(chan struct{})
		stopWhileSendHeld(t, r, func() {
			go func() {
				defer close(answered)
				var got []said
				_ = r.concierge.Answer(context.Background(), &r.exchange, "ask the agent on ABC-12 when it will be ready?", recorder(&got))
			}()
			select {
			case <-answered:
			case <-time.After(5 * time.Second):
				t.Fatal("control: the answer did not end while its send was held")
			}
			close(finished)
		}, sending, release, finished)
	})
}

// TestAStoppedConciergeTakesNoCall fails if a call that may write the store,
// made after Stop, runs: an answer, a tool call, a spoken mark or a never
// mind (storeguard records any write).
func TestAStoppedConciergeTakesNoCall(t *testing.T) {
	r := newRig(t)
	r.name(t, "agent-7")
	guard := storeguard.New(t, r.store)
	r.concierge.Stop()
	guard.Close(t)
	if err := r.concierge.Answer(context.Background(), &r.exchange, "ask the agent on ABC-12", recorder(&[]said{})); !errors.Is(err, voice.ErrUnreachable) || !errors.Is(err, errStopped) {
		t.Errorf("Answer after Stop = %v, want the stop, wrapping voice.ErrUnreachable", err)
	}
	raw, _ := json.Marshal(map[string]string{"session": "agent-7", "question": "q"})
	if result, err := r.concierge.Tools(&r.exchange).Run(context.Background(), voice.ToolCall{Name: "ask_agent", Args: raw}); !errors.Is(err, errStopped) || result != nil {
		t.Errorf("Run after Stop = %s, %v; want no answer and the stop", result, err)
	}
	if err := r.concierge.Spoken(context.Background(), "evt-1"); !errors.Is(err, errStopped) {
		t.Errorf("Spoken after Stop = %v, want the stop", err)
	}
	if cancelled, err := r.concierge.NeverMind(context.Background(), "meeting-1", r.concierge.QuestionsAsked()); cancelled || !errors.Is(err, errStopped) {
		t.Errorf("NeverMind after Stop = %v, %v; want nothing cancelled and the stop", cancelled, err)
	}
	if len(r.agents.attempts) != 0 {
		t.Errorf("%d sends after Stop, want none", len(r.agents.attempts))
	}
	guard.Check(t)
}
