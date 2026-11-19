package envoy

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeNATS is the least of a NATS server a client connects, subscribes and
// flushes against: it sends INFO, answers every PING with PONG, and records
// the subjects subscribed.
type fakeNATS struct {
	listener net.Listener
	mu       sync.Mutex
	subs     []string
}

func newFakeNATS(t *testing.T) *fakeNATS {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &fakeNATS{listener: listener}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go server.serve(conn)
		}
	}()
	return server
}

func (f *fakeNATS) url() string { return "nats://" + f.listener.Addr().String() }

func (f *fakeNATS) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write([]byte(`INFO {"server_id":"fake","version":"2.10.0","proto":1,"max_payload":1048576,"headers":true}` + "\r\n")); err != nil {
		return
	}
	lines := bufio.NewScanner(conn)
	for lines.Scan() {
		line := lines.Text()
		switch {
		case strings.HasPrefix(line, "PING"):
			if _, err := conn.Write([]byte("PONG\r\n")); err != nil {
				return
			}
		case strings.HasPrefix(line, "SUB "):
			f.mu.Lock()
			f.subs = append(f.subs, strings.Fields(line)[1])
			f.mu.Unlock()
		}
	}
}

func (f *fakeNATS) subjects() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.subs...)
}

// TestOpenSessionNeedsNoDeadlineFromItsCaller: a caller's context with no
// deadline opens the session; the flush and the first registration bound
// themselves. Live, a context with no deadline failed: "flush the relay
// subscription: nats: context requires a deadline".
func TestOpenSessionNeedsNoDeadlineFromItsCaller(t *testing.T) {
	nats := newFakeNATS(t)
	fake, server := newFakeListener(t)
	fake.answer("/v1/interests/subscribe", http.StatusOK, `{"session_id":"pi-session-1","delivery":{"mode":"consumer","stream_id":"stream-A","consumer_start":1}}`)
	listener, err := NewListener(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	session, err := OpenSession(context.Background(), listener, &SessionConfig{SessionID: "pi-session-1", NATSURL: nats.url(), Plugin: "ghost-wispr"})
	if err != nil {
		t.Fatalf("OpenSession with no deadline: %v", err)
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	defer func() { _ = session.Close(closeCtx) }()
	if subjects := nats.subjects(); len(subjects) != 1 || subjects[0] != DeliverSubjectPrefix+"pi-session-1" {
		t.Fatalf("subscribed to %q", subjects)
	}
	if body := fake.lastBody(t, "/v1/interests/subscribe"); body["session_id"] != "pi-session-1" {
		t.Fatalf("registration = %v", body)
	}
}
