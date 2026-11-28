package concierge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sjawhar/ghost-wispr/internal/voice"
	"github.com/sjawhar/ghost-wispr/internal/voice/dgagent"
	"github.com/sjawhar/ghost-wispr/internal/voice/gptlive"
)

// Sol's fifteenth review of #191 at 3646053
// (https://github.com/sjawhar/ghost-wispr/pull/191#pullrequestreview-6069027963),
// from Sol's probes, through the real Supervisor, Concierge and vendor
// clients, with a loopback vendor socket.

// vendorWire is one loopback vendor connection: what the client sent, frame
// by frame, and a way to send it events.
type vendorWire struct {
	conn   *websocket.Conn
	mu     sync.Mutex
	frames chan []byte
}

func (w *vendorWire) send(v any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.conn.WriteJSON(v)
}

// next is the next frame of type typ the client sent.
func (w *vendorWire) next(t *testing.T, typ string) []byte {
	t.Helper()
	timer := time.NewTimer(7 * time.Second)
	defer timer.Stop()
	for {
		select {
		case b := <-w.frames:
			var m struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(b, &m) == nil && m.Type == typ {
				return b
			}
		case <-timer.C:
			t.Fatalf("no %s reached the loopback vendor", typ)
			return nil
		}
	}
}

// loopbackVendor serves one vendor socket, answering Deepgram's or GPT-Live's
// opening as the vendor does, and hands its connection out.
func loopbackVendor(t *testing.T, deepgram bool) (string, <-chan *vendorWire) {
	t.Helper()
	conns := make(chan *vendorWire, 1)
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		c, err := upgrader.Upgrade(rw, req, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		w := &vendorWire{conn: c, frames: make(chan []byte, 512)}
		conns <- w
		if deepgram {
			w.send(map[string]any{"type": "Welcome"})
		}
		for {
			kind, b, err := c.ReadMessage()
			if err != nil {
				return
			}
			if kind != websocket.TextMessage {
				continue
			}
			var msg map[string]any
			if json.Unmarshal(b, &msg) != nil {
				return
			}
			switch typ, _ := msg["type"].(string); typ {
			case "Settings":
				w.send(map[string]any{"type": "SettingsApplied"})
			case "session.start":
				w.send(map[string]any{"type": "session.started", "event_id": "started", "session": map[string]any{"id": "loopback", "model": "gpt-live-1"}})
			case "session.instructions.append", "session.thinking.append", "session.commentary.append":
				w.send(map[string]any{"type": strings.TrimSuffix(typ, "append") + "appended", "event_id": "ack", "client_event_id": msg["event_id"], "start_ms": 0, "end_ms": 0})
			}
			w.frames <- append([]byte(nil), b...)
		}
	}))
	t.Cleanup(server.Close)
	return "ws" + strings.TrimPrefix(server.URL, "http"), conns
}

// watchedVoice is a vendor voice whose Open is reported, and whose answer
// tool calls and results are. starts counts the speech starts of its
// sessions the Supervisor has received (watchedSession).
type watchedVoice struct {
	inner   voice.Voice
	opened  chan struct{}
	calls   chan voice.ToolCall
	results chan json.RawMessage
	starts  atomic.Int64
}

func watch(v voice.Voice) *watchedVoice {
	return &watchedVoice{inner: v, opened: make(chan struct{}, 1), calls: make(chan voice.ToolCall, 8), results: make(chan json.RawMessage, 8)}
}

func (*watchedVoice) DrivesTurn() bool { return true }

func (v *watchedVoice) Open(ctx context.Context, x *voice.Exchange) (voice.Session, error) {
	opened := *x
	opened.Tools = watchedTools{inner: x.Tools, v: v}
	s, err := v.inner.Open(ctx, &opened)
	if err != nil {
		return s, err
	}
	v.opened <- struct{}{}
	return watchSession(ctx, s, v), nil
}

// watchedSession hands its session's events to the Supervisor one at a time,
// on a channel with no buffer, and counts each speech start the Supervisor
// received (watchedVoice.starts). The Supervisor reads a session's events in
// order on one goroutine, so once it has received a start it has taken in
// every event before it. Once ctx, the session's, ends, events no longer
// read are dropped, until the session's own channel closes.
type watchedSession struct {
	voice.Session
	events chan voice.Event
}

func watchSession(ctx context.Context, s voice.Session, v *watchedVoice) *watchedSession {
	w := &watchedSession{Session: s, events: make(chan voice.Event)}
	go func() {
		defer close(w.events)
		for e := range s.Events() {
			select {
			case w.events <- e:
				if e.Kind == voice.EventSpeechStarted {
					v.starts.Add(1)
				}
			case <-ctx.Done():
			}
		}
	}()
	return w
}

func (w *watchedSession) Events() <-chan voice.Event { return w.events }

type watchedTools struct {
	inner voice.ToolRunner
	v     *watchedVoice
}

func (w watchedTools) Run(ctx context.Context, c voice.ToolCall) (json.RawMessage, error) {
	w.v.calls <- c
	b, err := w.inner.Run(ctx, c)
	w.v.results <- b
	return b, err
}

func within[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(7 * time.Second):
		t.Fatalf("no %s", what)
		var zero T
		return zero
	}
}

// TestVoiceCsLateAskerWordsLeaveOutATakeBack (Sol's fifteenth review of
// #191, P2 1): voice C sends the asker's words as text when Deepgram heard
// no turn a while after it opened. At 3646053 it sent the words of its
// opening, "what about ABC-42?", though the room had said "never mind. what
// about ABC-12?" since. It now reads the room's words when it sends them.
// Controls: with no take-back it sends ABC-42; with the take-back said before
// it opened it sends ABC-12.
func TestVoiceCsLateAskerWordsLeaveOutATakeBack(t *testing.T) {
	for _, mode := range []string{"a take-back after the voice opened", "control: no take-back", "control: a take-back before the voice opened"} {
		t.Run(mode, func(t *testing.T) {
			url, conns := loopbackVendor(t, true)
			agent, err := dgagent.New(&dgagent.Config{APIKey: "loopback-not-a-secret", URL: url})
			if err != nil {
				t.Fatal(err)
			}
			v := watch(agent)
			r := openingRig(t, v)
			opening := "Wispr, what about ABC-42?"
			if mode == "control: a take-back before the voice opened" {
				opening += " never mind. what about ABC-12?"
			}
			r.hearNow(opening, false)
			w := within(t, conns, "vendor connection")
			w.next(t, "Settings")
			within(t, v.opened, "opened voice")
			if mode == "a take-back after the voice opened" {
				r.hearNow("never mind. what about ABC-12?", false)
			}
			var inject struct {
				Content string `json:"content"`
			}
			if err := json.Unmarshal(w.next(t, "InjectUserMessage"), &inject); err != nil {
				t.Fatal(err)
			}
			cancelled := strings.Contains(inject.Content, "ABC-42")
			if want := mode == "control: no take-back"; cancelled != want {
				t.Errorf("the asker's words sent late hold ABC-42: %v, want %v: %q", cancelled, want, inject.Content)
			}
		})
	}
}

// TestVoiceBsNextDelegationLeavesOutATakeBack (Sol's fifteenth review of
// #191, P2 2): at 3646053 a delegation that heard nothing new asked its last
// question again, whole, the asker's words of its opening "what about
// ABC-42?" included, and the next delegation after "never mind. what about
// ABC-12?" asked the model and the holder "what about ABC-42? what about
// ABC-12?". A question is now the room's own request, owned by final: a
// delegation that heard nothing new asks nothing and waits (Main, after
// Sol's nineteenth review of #191), and a take-back said between the two
// delegations, a final of its own, leaves the second without ABC-42. Control:
// with no take-back, the question keeps ABC-42.
func TestVoiceBsNextDelegationLeavesOutATakeBack(t *testing.T) {
	for _, tc := range []struct {
		name   string
		before string // the room's final after the voice opened
		heard  string // what GPT-Live heard before the first delegation
		after  string // the room's final between the two delegations
		cut    []bool // whether each delegation's question leaves ABC-42 out
	}{
		{"a take-back after the voice opened", "never mind. what about ABC-12?", "what about ABC-12?", "", []bool{true, true}},
		{"a take-back between the delegations", "", "", "never mind. what about ABC-12?", []bool{false, true}},
		{"control: no take-back", "what about ABC-12?", "what about ABC-12?", "", []bool{false, false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			if tc.before != "" {
				r.hearNow(tc.before, false)
			}
			if tc.heard != "" {
				w.send(map[string]any{"type": "session.input_transcript.delta", "event_id": "heard", "delta": tc.heard, "start_ms": 0, "end_ms": 100})
			}
			for i, id := range []string{"d1", "d2"} {
				if i == 1 && tc.after != "" {
					r.hearNow(tc.after, false)
				}
				sent := len(holderMessages(r))
				w.send(map[string]any{"type": "session.delegation.created", "event_id": "delegate-" + id, "offset_ms": 1000 * (i + 1), "delegation": map[string]any{"id": id, "type": "delegation", "target": "client"}})
				if i == 1 && tc.after == "" {
					// No new final: the room has nothing for d2 to ask.
					if q := toolWithin(t, v, 600*time.Millisecond); q != "" {
						t.Errorf("delegation d2 asked %q with no new final of the room's", q)
					}
					continue
				}
				within(t, v.calls, "delegation "+id)
				within(t, v.results, "answer to "+id)
				if got := strings.Contains(modelQuestion(r), "ABC-42"); got == tc.cut[i] {
					t.Errorf("delegation %s asked the model %q: holds ABC-42 %v, want %v", id, modelQuestion(r), got, !tc.cut[i])
				}
				for _, m := range holderMessages(r)[sent:] {
					if tc.cut[i] && strings.Contains(m, "ABC-42") {
						t.Errorf("delegation %s sent the holder the cancelled words: %q", id, m)
					}
				}
			}
		})
	}
}
