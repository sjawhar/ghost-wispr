package joined

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/envoy"
)

// The tests here hold the roster's memory of commands to Envoy's: a command
// is remembered for as long as Envoy can deliver it again (replayWindow), and
// an agent's floor and pointer limits for as long as they run, however many
// other commands and agents come meanwhile; and a command the roster cannot
// take yet is never acknowledged, so Envoy delivers it again.

// TestACommandIsRememberedForEnvoysWindow fails if a join Envoy delivers again
// acts after the agent left, once more later commands of that agent, or more
// other agents, have come than a bounded count would keep; or if those other
// agents reset the agent's floor and pointer limits. Past the window, a
// sweep forgets what Envoy can no longer deliver: the roster's memory stays
// bounded by time.
func TestACommandIsRememberedForEnvoysWindow(t *testing.T) {
	r := newRig(t, func(cfg *Config) { cfg.Logf = func(string, ...any) {} })
	join := r.deliver(t, "agent-1", "ghost-wispr join ABC-12")
	r.deliver(t, "agent-1", "ghost-wispr floor\nThe first update is ready.")
	r.roster.OnSegment(r.segment("ABC-12 is ready.", false))
	r.roster.wg.Wait()
	if len(r.floor.hands()) != 1 || len(r.pointers("agent-1")) != 1 {
		t.Fatal("control: the first floor request and pointer did not go")
	}
	r.deliver(t, "agent-1", "ghost-wispr leave")
	for range 100 { // refused floor requests are commands too
		r.deliver(t, "agent-1", "ghost-wispr floor\nThe update is ready.")
	}
	for i := range 300 {
		agent := fmt.Sprintf("other-%d", i)
		r.join(t, agent, "XYZ-9")
		r.deliver(t, agent, "ghost-wispr leave")
	}
	if _, err := r.roster.Deliver(context.Background(), join); err != nil {
		t.Fatal(err)
	}
	if got := r.roster.Joined(); len(got) != 0 {
		t.Fatalf("the join delivered again, after 101 later commands and 300 other agents, made a membership: %+v", got)
	}
	r.join(t, "agent-1", "ABC-12")
	r.deliver(t, "agent-1", "ghost-wispr floor\nThe second update is ready.")
	r.roster.OnSegment(r.segment("ABC-12 is ready again.", false))
	r.roster.wg.Wait()
	if floors, pointers := len(r.floor.hands()), len(r.pointers("agent-1")); floors != 1 || pointers != 1 {
		t.Fatalf("300 other agents reset agent-1's limits: %d floor requests and %d pointers, want 1 and 1", floors, pointers)
	}

	r.deliver(t, "agent-1", "ghost-wispr leave")
	r.clock.advance(replayWindow + time.Minute)
	r.roster.Sweep(context.Background())
	r.roster.mu.Lock()
	kept := len(r.roster.history)
	r.roster.mu.Unlock()
	if kept != 0 {
		t.Fatalf("the roster kept the history of %d agents past Envoy's window, with no membership and no limit running", kept)
	}
}

// relayFake is a NATS server, for the real NATS client of envoy.Session: it
// sends the frames a test gives it, and records each terminal answer the
// session sends back (done or nak).
type relayFake struct {
	listener net.Listener
	conns    chan *relayConn
}

type relayConn struct {
	net.Conn
	mu           sync.Mutex
	subject, sid string
	answers      chan string
}

func (c *relayConn) send(data string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err := io.WriteString(c.Conn, data)
	return err
}

func newRelayFake(t *testing.T) *relayFake {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &relayFake{listener: l, conns: make(chan *relayConn, 8)}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			raw, err := l.Accept()
			if err != nil {
				return
			}
			go f.serve(raw)
		}
	}()
	return f
}

func (f *relayFake) serve(raw net.Conn) {
	c := &relayConn{Conn: raw, answers: make(chan string, 64)}
	defer raw.Close()
	if c.send("INFO {\"server_id\":\"relay-fake\",\"version\":\"2.10.0\",\"proto\":1,\"max_payload\":1048576,\"headers\":true}\r\n") != nil {
		return
	}
	r := bufio.NewReader(raw)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "SUB":
			c.subject, c.sid = fields[1], fields[len(fields)-1]
			f.conns <- c
		case "PING":
			if c.send("PONG\r\n") != nil {
				return
			}
		case "PUB", "HPUB":
			size, err := strconv.Atoi(fields[len(fields)-1])
			if err != nil {
				return
			}
			body := make([]byte, size+2)
			if _, err := io.ReadFull(r, body); err != nil {
				return
			}
			if answer := string(body[:size]); fields[0] == "PUB" && (answer == "done" || answer == "nak") {
				c.answers <- answer
			}
		}
	}
}

func (f *relayFake) next(t *testing.T) *relayConn {
	t.Helper()
	select {
	case c := <-f.conns:
		return c
	case <-time.After(8 * time.Second):
		t.Fatal("the NATS client did not subscribe")
		return nil
	}
}

// frame sends one relay frame of stream seq, as Envoy's listener does.
func (c *relayConn) frame(t *testing.T, seq int, id, agent, text string, issued time.Time) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"event_id": id, "source_session": agent, "payload": text, "issued_at": issued.UnixMilli()})
	if err != nil {
		t.Fatal(err)
	}
	header := fmt.Sprintf("NATS/1.0\r\nEnvoy-Stream-Id: stream-A\r\nEnvoy-Stream-Seq: %d\r\nEnvoy-Subject: notifications.agent.unit\r\n\r\n", seq)
	if err := c.send(fmt.Sprintf("HMSG %s %s _INBOX.relay %d %d\r\n%s%s\r\n", c.subject, c.sid, len(header), len(header)+len(body), header, body)); err != nil {
		t.Fatal(err)
	}
}

func (c *relayConn) answer(t *testing.T) string {
	t.Helper()
	select {
	case a := <-c.answers:
		return a
	case <-time.After(5 * time.Second):
		t.Fatal("the session answered the frame neither done nor nak")
		return ""
	}
}

// TestAJoinDuringRegistrationIsNotAcknowledgedUntilTaken fails if a join that
// reaches the unit's Envoy session while it registers, before the roster has
// the session, is answered done (so Envoy never delivers it again, and the
// resume point moves past it) though the roster could not take it. It must be
// answered nak, with the resume point below it; delivered again once the
// roster has the session, as Envoy's listener does, it joins and is answered
// done.
func TestAJoinDuringRegistrationIsNotAcknowledgedUntilTaken(t *testing.T) {
	r := newRig(t, nil)
	r.roster.SetAgents(nil)
	relay := newRelayFake(t)
	var conn *relayConn
	early := make(chan string, 1)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if q.URL.Path == "/v1/interests/subscribe" {
			if conn == nil { // the first registration: a join is already queued for the session
				conn = relay.next(t)
				conn.frame(t, 1, "join-early", "agent-1", "ghost-wispr join ABC-12", r.clock.Now())
				early <- conn.answer(t)
			}
			_, _ = io.WriteString(w, `{"delivery":{"stream_id":"stream-A","consumer_start":1}}`)
			return
		}
		_, _ = io.WriteString(w, `{"state":"live","title":"Worker","event_id":"sent","recipient_state":"live"}`)
	}))
	defer hub.Close()
	listener, err := envoy.NewListener(hub.URL, hub.Client())
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var saved []envoy.ResumePoint
	s, err := envoy.OpenSession(t.Context(), listener, &envoy.SessionConfig{
		SessionID: "unit-session",
		NATSURL:   "nats://" + relay.listener.Addr().String(),
		Deliver: func(ctx context.Context, m *envoy.Message) error {
			_, err := r.roster.Deliver(ctx, m)
			return err
		},
		SaveResume: func(p envoy.ResumePoint) error {
			mu.Lock()
			defer mu.Unlock()
			saved = append(saved, p)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	if got := <-early; got != "nak" {
		t.Fatalf("the join that came while the roster had no Envoy session was answered %q, want nak so that it comes again", got)
	}
	mu.Lock()
	for _, p := range saved {
		if p.Seq >= 1 {
			t.Errorf("the resume point moved to %d, past the join the roster did not take", p.Seq)
		}
	}
	mu.Unlock()
	r.roster.SetAgents(s)
	conn.frame(t, 1, "join-early", "agent-1", "ghost-wispr join ABC-12", r.clock.Now()) // Envoy delivers it again
	if got := conn.answer(t); got != "done" {
		t.Fatalf("the join delivered again once the roster had its session was answered %q, want done", got)
	}
	if got := r.roster.Joined(); len(got) != 1 || got[0].Agent != "agent-1" {
		t.Fatalf("roster %+v after the join came again, want agent-1 joined", got)
	}
}
