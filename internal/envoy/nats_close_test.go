package envoy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// stallingNATS is a NATS server on a real TCP socket that, with stall set,
// stops reading once the client's first publish reaches it, until release
// closes: the client's writes then block in the socket. It returns its
// address and the accepted connection, and published, which closes at that
// first publish.
func stallingNATS(t *testing.T, stall bool, release <-chan struct{}) (addr string, accepted <-chan net.Conn, published <-chan struct{}) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	conns, first := make(chan net.Conn, 1), make(chan struct{})
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		if stall {
			_ = conn.(*net.TCPConn).SetReadBuffer(1024)
		}
		conns <- conn
		_, _ = fmt.Fprint(conn, "INFO {\"server_id\":\"test\",\"version\":\"2.10.0\",\"proto\":1,\"max_payload\":1048576}\r\n")
		lines := bufio.NewReader(conn)
		seenPub := false
		for {
			line, err := lines.ReadString('\n')
			if err != nil {
				return
			}
			if strings.HasPrefix(line, "PING") {
				_, _ = fmt.Fprint(conn, "PONG\r\n")
			}
			if strings.HasPrefix(line, "PUB ") {
				if !seenPub {
					seenPub = true
					close(first)
					if stall {
						<-release
					}
				}
				fields := strings.Fields(line)
				size, _ := strconv.Atoi(fields[len(fields)-1])
				if _, err := io.CopyN(io.Discard, lines, int64(size+2)); err != nil {
					return
				}
			}
		}
	}()
	return "nats://" + listener.Addr().String(), conns, first
}

// natsWriteBlocked reports whether a goroutine is blocked writing to a
// socket inside the NATS client.
func natsWriteBlocked() bool {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	for _, stack := range strings.Split(string(buf[:n]), "\n\n") {
		if strings.Contains(stack, "internal/poll.(*FD).Write") && strings.Contains(stack, "nats.go") {
			return true
		}
	}
	return false
}

// TestCloseStopsWaitingForANATSCloseAtItsDeadline fails if Close, its
// context already done, waits for the NATS connection's close while a write
// to a peer that stopped reading holds the connection: NATS's Close flushes
// under the lock that write holds, and the shutdown would wait for the peer.
// The control's peer reads, and its writes drain before Close. From review
// 6006350418 of #132.
func TestCloseStopsWaitingForANATSCloseAtItsDeadline(t *testing.T) {
	for _, stall := range []bool{false, true} {
		name := "control: the peer reads"
		if stall {
			name = "the peer stopped reading, and a write blocks in the socket"
		}
		t.Run(name, func(t *testing.T) {
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			natsURL, accepted, published := stallingNATS(t, stall, release)
			fake, server := newFakeListener(t)
			fake.answer("/v1/interests/subscribe", http.StatusOK, `{"delivery":{"stream_id":"stream-A","consumer_start":1}}`)
			listener, err := NewListener(server.URL, server.Client())
			if err != nil {
				t.Fatal(err)
			}
			session, err := OpenSession(context.Background(), listener, &SessionConfig{SessionID: "pi-session-1", NATSURL: natsURL})
			if err != nil {
				t.Fatal(err)
			}
			peer := <-accepted
			defer func() { _ = peer.Close() }()
			sent := make(chan struct{})
			go func() {
				defer close(sent)
				payload := make([]byte, 1<<20)
				for range 16 {
					if session.conn.Publish("pi-session-1.reply", payload) != nil {
						return
					}
				}
			}()
			select {
			case <-published:
			case <-time.After(5 * time.Second):
				t.Fatal("control: no publish reached the peer")
			}
			if stall {
				deadline := time.Now().Add(5 * time.Second)
				for !natsWriteBlocked() {
					if time.Now().After(deadline) {
						t.Fatal("control: no NATS write blocked in the socket")
					}
					time.Sleep(time.Millisecond)
				}
			} else {
				select {
				case <-sent:
				case <-time.After(5 * time.Second):
					t.Fatal("control: the publishes did not drain")
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel() // the shutdown's deadline has passed
			closed := make(chan error, 1)
			go func() { closed <- session.Close(ctx) }()
			select {
			case <-closed:
			case <-time.After(300 * time.Millisecond):
				t.Error("Close, its deadline passed, waited for the NATS close behind a write to a peer that stopped reading")
				_ = peer.Close()
				<-closed
			}
			unblock()
			select {
			case <-sent:
			case <-time.After(10 * time.Second):
				t.Fatal("the publishes did not end")
			}
		})
	}
}

// TestTheNATSConnectionsWritesHaveATimeout fails if the session's NATS
// connection keeps the client's default write timeout of a minute: a peer
// that stops reading would hold each write, flush and close that long.
func TestTheNATSConnectionsWritesHaveATimeout(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	natsURL, _, _ := stallingNATS(t, false, release)
	fake, server := newFakeListener(t)
	fake.answer("/v1/interests/subscribe", http.StatusOK, `{"delivery":{"stream_id":"stream-A","consumer_start":1}}`)
	listener, err := NewListener(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	session, err := OpenSession(context.Background(), listener, &SessionConfig{SessionID: "pi-session-1", NATSURL: natsURL})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close(context.Background()) }()
	if got := session.conn.Opts.FlusherTimeout; got != natsWriteTimeout {
		t.Errorf("the NATS connection's write timeout is %v, want %v", got, natsWriteTimeout)
	}
}

// heldWarn is a log handler that holds the record whose message is match
// until release closes; entered closes when it comes.
type heldWarn struct {
	match            string
	entered, release chan struct{}
	once             sync.Once
}

func (h *heldWarn) Enabled(context.Context, slog.Level) bool { return true }

//nolint:gocritic // slog.Handler's Handle takes the record by value.
func (h *heldWarn) Handle(_ context.Context, r slog.Record) error {
	if r.Message == h.match {
		h.once.Do(func() { close(h.entered) })
		<-h.release
	}
	return nil
}

func (h *heldWarn) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *heldWarn) WithGroup(string) slog.Handler      { return h }

// TestAFailedResumeSaveLogsWithoutHoldingTheNextSave fails if a resume
// point's save that failed writes its log record while it holds saveMu: a
// log write can wait, on a journal that does not read, and every later save
// would wait behind it. From the lock audit of review 6006350418 of #132.
func TestAFailedResumeSaveLogsWithoutHoldingTheNextSave(t *testing.T) {
	_, server := newFakeListener(t)
	session := testSession(t, server, nil, nil)
	held := &heldWarn{match: "envoy session could not save its resume point", entered: make(chan struct{}), release: make(chan struct{})}
	defer close(held.release)
	session.logger = slog.New(held)
	var saves atomic.Int32
	secondSaved := make(chan struct{})
	session.cfg.SaveResume = func(ResumePoint) error {
		if saves.Add(1) == 1 {
			return errors.New("disk I/O error")
		}
		close(secondSaved)
		return nil
	}
	go session.wrote("stream-A", 1) // its save fails, and its log write waits
	select {
	case <-held.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("control: the failed save logged nothing")
	}
	go session.wrote("stream-A", 2)
	select {
	case <-secondSaved:
	case <-time.After(time.Second):
		t.Fatal("the next save waited behind the failed save's log write")
	}
}
