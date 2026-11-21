package main

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/concierge"
	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/envoy"
)

// localNATS is a NATS server enough for a client to connect and stay
// connected: it greets each connection and answers each PING. It returns its
// address and each connection as it is accepted, so a test can close one and
// make the client reconnect.
func localNATS(t *testing.T) (string, <-chan net.Conn) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan net.Conn, 16)
	var mu sync.Mutex
	var all []net.Conn
	t.Cleanup(func() {
		_ = listener.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range all {
			_ = c.Close()
		}
	})
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			all = append(all, conn)
			mu.Unlock()
			accepted <- conn
			go func() {
				defer func() { _ = conn.Close() }()
				_, _ = fmt.Fprint(conn, "INFO {\"server_id\":\"test\",\"version\":\"2.10.0\",\"proto\":1,\"max_payload\":1048576}\r\n")
				lines := bufio.NewScanner(conn)
				for lines.Scan() {
					if strings.TrimSpace(lines.Text()) == "PING" {
						_, _ = fmt.Fprint(conn, "PONG\r\n")
					}
				}
			}()
		}
	}()
	return "nats://" + listener.Addr().String(), accepted
}

// closeTracked is an Envoy session that closes closed once its Close has
// returned.
type closeTracked struct {
	*envoy.Session
	closed chan struct{}
}

func (s *closeTracked) Close(ctx context.Context) error {
	defer close(s.closed)
	return s.Session.Close(ctx)
}

// TestARunningConciergesEnvoyResumeWriteDoesNotHoldTheShutdown fails if the
// shutdown of a running Concierge with an open Envoy session
// (lazyConcierge.Stop) waits past its deadline while a NATS reconnect's
// registration writes the resume point to the real store, whose only
// connection another holds: the write waits for the connection and sees no
// cancel, and main could not reach closeStore. It also fails if the
// shutdown, its deadline already past, returns without closing the open
// session. The control lets the write land first. From review 6006047326 of
// #132.
func TestARunningConciergesEnvoyResumeWriteDoesNotHoldTheShutdown(t *testing.T) {
	for _, held := range []bool{false, true} {
		name := "control: the resume write lands first"
		if held {
			name = "the reconnect's resume write waits for the store's connection"
		}
		t.Run(name, func(t *testing.T) {
			natsURL, connections := localNATS(t)
			store := conciergeStore(t)
			// The reconnect's registration, the second, notes the store's
			// waits as it answers: its resume write comes after the answer,
			// so a wait past that count is the resume write's.
			var registrations atomic.Int32
			var waitsAtReconnect atomic.Int64
			reregistered := make(chan struct{})
			listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodDelete {
					_, _ = fmt.Fprint(w, `{}`)
					return
				}
				n := registrations.Add(1)
				if n == 2 {
					waitsAtReconnect.Store(store.DB().Stats().WaitCount)
					close(reregistered)
				}
				_, _ = fmt.Fprintf(w, `{"delivery":{"stream_id":"stream-%d","consumer_start":%d}}`, n, n*10)
			}))
			defer listener.Close()
			original := openEnvoySession
			t.Cleanup(func() { openEnvoySession = original })
			opened := make(chan struct{})
			tracked := &closeTracked{closed: make(chan struct{})}
			openEnvoySession = func(ctx context.Context, l *envoy.Listener, cfg *envoy.SessionConfig) (envoySession, error) {
				local := *cfg
				local.NATSURL = natsURL
				session, err := envoy.OpenSession(ctx, l, &local)
				if err != nil {
					return nil, err
				}
				tracked.Session = session
				close(opened)
				return tracked, nil
			}
			cfg := config.Config{GenAIBackend: "gemini", GeminiAPIKey: "test", Concierge: config.Concierge{EnvoyURL: listener.URL}}
			lazy := newLazyConcierge(func() (*concierge.Concierge, func(context.Context), error) {
				return startConcierge(t.Context(), &cfg, store, discardLogger, nil, nil)
			}, nil, discardLogger)
			if _, err := lazy.start(); err != nil {
				t.Fatal(err)
			}
			select {
			case <-opened:
			case <-time.After(5 * time.Second):
				t.Fatal("control: the Envoy session did not open")
			}
			var first net.Conn
			select {
			case first = <-connections:
			case <-time.After(5 * time.Second):
				t.Fatal("control: no NATS connection")
			}
			conn, err := store.DB().Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			_ = first.Close() // the client reconnects, and registers again
			select {
			case <-reregistered:
			case <-time.After(10 * time.Second):
				t.Fatal("control: the reconnect did not register")
			}
			waitUntil(t, "the reconnect's resume write waiting for the store's connection", func() bool {
				return store.DB().Stats().WaitCount > waitsAtReconnect.Load()
			})
			if !held {
				_ = conn.Close()
				waitUntil(t, "the reconnect's resume write landing", func() bool {
					return store.DB().Stats().WaitDuration > 0 && store.DB().Stats().InUse == 0
				})
			}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			<-ctx.Done() // the shutdown's earlier steps took the whole deadline
			stopped := make(chan struct{})
			go func() {
				defer close(stopped)
				lazy.Stop(ctx)
			}()
			select {
			case <-stopped:
			case <-time.After(time.Second):
				t.Fatal("the Concierge's shutdown waited past its deadline behind the Envoy session's resume write")
			}
			select {
			case <-tracked.closed:
			default:
				t.Error("the shutdown, past its deadline, returned without closing the open Envoy session")
			}
		})
	}
}
