package call

import (
	"bytes"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// fakeEnd is one end of an in-memory link: what one end writes, the other
// reads, in order. It has no buffer: WriteMessage returns once the other end
// has read the message, so a test knows that whatever it queues while a
// message is unread waits in the sender's queue.
type fakeEnd struct {
	out  chan<- fakeMessage
	in   <-chan fakeMessage
	link *fakeLink
}

// fakeLink is what both ends share: closing it ends every write, as closing
// a WebSocket connection does.
type fakeLink struct {
	done chan struct{}
	once sync.Once
}

type fakeMessage struct {
	kind int
	data []byte
}

// newFakeTransport returns the two ends of one link, closed at the test's end.
func newFakeTransport(t *testing.T) (pi, bridge *fakeEnd) {
	t.Helper()
	toBridge := make(chan fakeMessage)
	toPi := make(chan fakeMessage)
	link := &fakeLink{done: make(chan struct{})}
	pi = &fakeEnd{out: toBridge, in: toPi, link: link}
	bridge = &fakeEnd{out: toPi, in: toBridge, link: link}
	t.Cleanup(pi.close)
	return pi, bridge
}

// errFakeClosed is what a write on a closed fake link returns.
var errFakeClosed = errors.New("fake transport: closed")

// close closes the link, so a write blocked on it returns an error.
func (e *fakeEnd) close() {
	e.link.once.Do(func() { close(e.link.done) })
}

// WriteMessage hands a copy of data to the other end: a gorilla/websocket
// Conn is done with data when WriteMessage returns, and the queue reuses it.
func (e *fakeEnd) WriteMessage(kind int, data []byte) error {
	select {
	case e.out <- fakeMessage{kind: kind, data: bytes.Clone(data)}:
		return nil
	case <-e.link.done:
		return errFakeClosed
	}
}

// read returns the next message the other end wrote, decoded as the link's
// reader decodes it: a text message as a control message, a binary one as a
// frame. It fails the test when none comes within a second.
func (e *fakeEnd) read(t *testing.T) Message {
	t.Helper()
	select {
	case m := <-e.in:
		switch m.kind {
		case websocket.TextMessage:
			c, err := ParseControl(m.data)
			if err != nil {
				t.Fatalf("a text message that is no control message: %v: %s", err, m.data)
			}
			return Message{Control: &c}
		case websocket.BinaryMessage:
			var f Frame
			if err := ParseFrame(m.data, &f); err != nil {
				t.Fatalf("a binary message that is no frame: %v", err)
			}
			return Message{Frame: &f}
		}
		t.Fatalf("a message of WebSocket type %d, neither text nor binary", m.kind)
	case <-time.After(time.Second):
		t.Fatal("no message came within 1 s")
	}
	return Message{}
}

// quiet fails the test if the other end writes anything within d.
func (e *fakeEnd) quiet(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case m := <-e.in:
		t.Fatalf("a message of type %d and %d bytes came; want none yet: %q", m.kind, len(m.data), m.data)
	case <-time.After(d):
	}
}
