package envoy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/storeguard"
)

// handshakeNATS is a local TCP peer that answers only the NATS connection
// handshake and PING: it sends no relay frame and takes no publish. Each
// connection it accepts is sent on the channel it returns, so a test can
// close one and make the client reconnect.
func handshakeNATS(t *testing.T) (url string, conns <-chan net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan net.Conn, 4)
	var mu sync.Mutex
	var all []net.Conn
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range all {
			_ = c.Close()
		}
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			all = append(all, c)
			mu.Unlock()
			accepted <- c
			go func() {
				defer func() { _ = c.Close() }()
				_, _ = fmt.Fprint(c, "INFO {\"server_id\":\"handshake\",\"version\":\"2.10.0\",\"proto\":1,\"max_payload\":1048576}\r\n")
				scanner := bufio.NewScanner(c)
				for scanner.Scan() {
					if strings.TrimSpace(scanner.Text()) == "PING" {
						_, _ = fmt.Fprint(c, "PONG\r\n")
					}
				}
			}()
		}
	}()
	return "nats://" + ln.Addr().String(), accepted
}

// TestAFailedOpenJoinsItsReconnectRegistration fails if OpenSession, whose
// first registration fails, returns while the registration a NATS reconnect
// began during it still runs: that registration saves its resume point after
// the unit closed its store (storeguard records it), and no Session is
// returned for the unit to close. From review 6004725672 of #132.
func TestAFailedOpenJoinsItsReconnectRegistration(t *testing.T) {
	db, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "open.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	guard := storeguard.New(t, db)
	firstRegister, reconnectRegister := make(chan struct{}), make(chan struct{})
	failFirst, releaseReconnect := make(chan struct{}), make(chan struct{})
	fail, release := sync.OnceFunc(func() { close(failFirst) }), sync.OnceFunc(func() { close(releaseReconnect) })
	t.Cleanup(fail)
	t.Cleanup(release)
	cancelled := make(chan struct{})
	var registrations atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusOK)
			return
		}
		// Read first: the server sees a client that went away only once the
		// request's body is read, and then ends r's context.
		_, _ = io.Copy(io.Discard, r.Body)
		switch registrations.Add(1) {
		case 1: // the first registration, which fails once the reconnect's is under way
			close(firstRegister)
			<-failFirst
			http.Error(w, "the first registration fails", http.StatusServiceUnavailable)
		case 2: // the reconnect's
			close(reconnectRegister)
			select {
			case <-r.Context().Done():
				close(cancelled)
				return
			case <-releaseReconnect:
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"delivery":{"stream_id":"reconnect-stream","consumer_start":9}}`)
		default:
			http.Error(w, "an unexpected registration", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(server.Close)
	listener, err := NewListener(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	natsURL, connections := handshakeNATS(t)
	saved := make(chan error, 1)
	opened := make(chan error, 1)
	go func() {
		s, err := OpenSession(context.Background(), listener, &SessionConfig{SessionID: "open-failure", NATSURL: natsURL, SaveResume: func(p ResumePoint) error {
			err := db.SetAppState("open-failure-resume", p.StreamID)
			saved <- err
			return err
		}})
		if s != nil {
			_ = s.Close(context.Background())
		}
		opened <- err
	}()
	select {
	case <-firstRegister:
	case <-time.After(5 * time.Second):
		t.Fatal("control: the first registration did not come")
	}
	var first net.Conn
	select {
	case first = <-connections:
	case <-time.After(5 * time.Second):
		t.Fatal("control: no NATS connection")
	}
	_ = first.Close() // the client reconnects, and its reconnect registers
	select {
	case <-reconnectRegister:
	case <-time.After(6 * time.Second):
		t.Fatal("control: the NATS reconnect did not register")
	}
	fail()
	select {
	case err := <-opened:
		if err == nil {
			t.Fatal("control: the first registration did not fail")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OpenSession did not return: it does not cancel the reconnect's registration it waits for")
	}
	guard.Close(t) // as the unit closes its store once the open failed
	// OpenSession returned only after its reconnect's registration ended, so
	// the listener sees that request cancelled. Release only after that, or
	// after it did not come, so the release cannot race the cancellation.
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		release() // let a registration the failed open left running end
		select {
		case err := <-saved:
			t.Fatalf("the reconnect's registration ran on after OpenSession failed, and saved its resume point (%v)", err)
		case <-time.After(5 * time.Second):
			t.Fatal("the reconnect's registration was neither cancelled nor ended")
		}
	}
	release()
	select {
	case err := <-saved:
		t.Errorf("the reconnect's registration saved its resume point after OpenSession failed (%v)", err)
	default:
	}
	guard.Check(t)
}
