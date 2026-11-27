// Sol's twenty-second review of #191 at 606c751
// (https://github.com/sjawhar/ghost-wispr/pull/191#pullrequestreview-6082473039):
// Sol's cause probes, kept as regression tests. Both fail at 606c751; the
// one setup line adapted is marked.

package gptlive

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/voice"
)

func sol22Await(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// All speech enters through the same loopback socket read by production.
// The write lock stands for a concurrent input-audio/control frame in send.
func TestSol191R22CauseAtTheActualSend(t *testing.T) {
	for _, mode := range []string{"control_no_client_send", "control_commentary_sent", "commentary_waiting_for_writer"} {
		t.Run(mode, func(t *testing.T) {
			out := newFakeOutput()
			x := testExchange(out, newFakeTools(voice.AnswerResult{}))
			var retired atomic.Int64
			x.Answered = func(time.Time) { retired.Add(1) }
			raw, c, _ := openSession(t, newLiveServer(t), x)
			s := raw.(*session)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			locked := false
			defer func() {
				if locked {
					s.writeMu.Unlock()
				}
			}()
			if mode == "commentary_waiting_for_writer" {
				s.writeMu.Lock()
				locked = true
			}
			if mode != "control_no_client_send" {
				go func() { done <- s.Say(ctx, "The held answer is that ABC-123 is merged.", voice.Answer) }()
				if locked {
					// Adapted (setup only): the commentary has reached appendAll and waits
					// for the writer. It records no cause there: a cause is recorded as
					// its frame is written (send), so the wait no longer requires one.
					sol22Await(t, "Say has reached appendAll", func() bool {
						s.mu.Lock()
						defer s.mu.Unlock()
						return len(s.pending) > 0
					})
					select {
					case ev := <-c.in:
						t.Fatalf("a client frame escaped the locked writer: %+v", ev)
					default:
					}
				} else {
					c.expect(evCommentaryAppend)
				}
			}
			c.audioDelta(ramp(3000, 2400))
			c.said("first speech reader barrier")
			waitEvent(t, s, voice.EventAgentText)
			first := retired.Load()
			wantFirst := int64(1)
			if mode == "control_commentary_sent" {
				wantFirst = 0
			}
			if first != wantFirst {
				t.Errorf("before commentary could be sent: direct-answer callbacks=%d, want %d", first, wantFirst)
			}
			waitEvent(t, s, voice.EventSpeechEnded)
			if locked {
				s.writeMu.Unlock()
				locked = false
				ev := c.expect(evCommentaryAppend)
				t.Logf("held commentary reached vendor only after the first speech: %q", ev.Content)
				c.audioDelta(ramp(6000, 2400))
				c.said("second speech reader barrier")
				waitEvent(t, s, voice.EventAgentText)
				second := retired.Load() - first
				if second != 0 {
					t.Errorf("speech after the held answer was actually sent: direct-answer callbacks=%d, want 0 (must preserve the new room question)", second)
				}
			}
			cancel()
			if mode != "control_no_client_send" {
				select {
				case err := <-done:
					t.Logf("Say ended: %v", err)
				case <-time.After(3 * time.Second):
					t.Fatal("Say did not stop")
				}
			}
		})
	}
}

// Back-to-back vendor frames exercise both possible receive orders without
// inventing two concurrent readers for a single WebSocket.
func TestSol191R22DelegationAndSpeechWireOrders(t *testing.T) {
	for _, speechFirst := range []bool{true, false} {
		name := "delegation_first"
		if speechFirst {
			name = "speech_first"
		}
		t.Run(name, func(t *testing.T) {
			tools := newFakeTools(voice.AnswerResult{})
			tools.block = true
			x := testExchange(newFakeOutput(), tools)
			room := withRoom(x, "what about ABC-123?")
			var retired atomic.Int64
			x.Answered = func(time.Time) { retired.Add(1); v := room.get(); v.Request = ""; room.set(v) }
			s, c, _ := openSession(t, newLiveServer(t), x)
			if speechFirst {
				c.audioDelta(ramp(3000, 2400))
				c.delegate("same")
			} else {
				c.delegate("same")
				c.audioDelta(ramp(3000, 2400))
			}
			c.said("ordered reader barrier")
			waitEvent(t, s, voice.EventAgentText)
			if speechFirst {
				if got := retired.Load(); got != 1 {
					t.Errorf("speech before delegation retired %d times", got)
				}
				select {
				case call := <-tools.calls:
					t.Errorf("self-answered question was sent: %s", call.Args)
				case <-time.After(100 * time.Millisecond):
				}
				room.set(voice.RoomView{Live: true, Request: "is ABC-123 merged?", Revision: 2})
				call := tools.call(t)
				var a voice.AnswerArgs
				_ = json.Unmarshal(call.Args, &a)
				if a.Question != "is ABC-123 merged?" {
					t.Errorf("next question=%q", a.Question)
				}
			} else {
				call := tools.call(t)
				var a voice.AnswerArgs
				_ = json.Unmarshal(call.Args, &a)
				if a.Question != "what about ABC-123?" {
					t.Errorf("open delegation question=%q", a.Question)
				}
				if got := retired.Load(); got != 0 {
					t.Errorf("speech during an open delegation retired %d times", got)
				}
			}
		})
	}
}
