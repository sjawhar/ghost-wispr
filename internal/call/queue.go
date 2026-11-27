package call

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/gorilla/websocket"
)

// MessageWriter writes one WebSocket message. A *websocket.Conn of
// github.com/gorilla/websocket is one: it is done with data when
// WriteMessage returns.
type MessageWriter interface {
	WriteMessage(messageType int, data []byte) error
}

// ErrQueueClosed is the error of every call on a SendQueue whose writer has
// ended: its link is down.
var ErrQueueClosed = errors.New("call: the link's send queue is closed")

// ErrQueueServing is the error of a Serve call made while another Serve of
// the same queue runs: a link has one writer.
var ErrQueueServing = errors.New("call: the link's send queue already has its writer")

// maxFrameBytes is the length of the longest frame, a 24 kHz one; each frame
// buffer has this capacity, so it takes a frame of any stream.
const maxFrameBytes = HeaderSize + 2*480

// SendQueue holds what one side writes on one link, and Serve writes it. The
// first message is the side's hello, and nothing queued before the hello goes
// out before it. After it, a waiting control message goes before every
// waiting frame; control messages keep their order, and so do frames. At
// most maxFrames frames wait: a new frame then drops the oldest, so a slow
// link holds no more than that much old audio. A queue serves one link; when
// Serve ends, every call returns ErrQueueClosed, and the next link takes a
// new queue. Its methods are safe to call from many goroutines.
type SendQueue struct {
	mu        sync.Mutex
	wake      chan struct{} // holds a token when a message came since Serve last looked
	hello     []byte        // the encoded hello; nil until Send takes it
	helloSent bool
	controls  [][]byte // encoded control messages, oldest first
	frames    [][]byte // a ring of encoded frames: n of them from head
	head, n   int
	spare     [][]byte // frame buffers to reuse
	dropped   int
	serving   bool // a Serve took the queue's writer
	closed    bool
}

// NewSendQueue returns a queue that holds at most maxFrames frames. It
// panics when maxFrames is less than 1.
func NewSendQueue(maxFrames int) *SendQueue {
	if maxFrames < 1 {
		panic(fmt.Sprintf("call: a send queue for %d frames", maxFrames))
	}
	return &SendQueue{wake: make(chan struct{}, 1), frames: make([][]byte, maxFrames)}
}

// Send queues a control message. A hello is the link's first message,
// whenever it is sent. Send refuses a second hello, and a control message
// MarshalJSON refuses, as a hello that names no link version. It keeps no
// reference to c.
func (q *SendQueue) Send(c *Control) error {
	isHello := c.Type == TypeHello
	b, err := c.MarshalJSON()
	if err != nil {
		return err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	switch {
	case q.closed:
		return ErrQueueClosed
	case !isHello:
		q.controls = append(q.controls, b)
	case q.hello != nil:
		return errors.New("call: a link has one hello")
	default:
		q.hello = b
	}
	q.signal()
	return nil
}

// SendAudio queues a frame, encoded at once, so the caller may reuse f.PCM.
// It refuses, with an error that wraps ErrBadFrame, a frame of an unknown
// stream or with other than its stream's 20 ms of samples. When maxFrames
// frames wait, it drops the oldest and counts it.
func (q *SendQueue) SendAudio(f Frame) error {
	if n := FrameSamples(f.Stream); n == 0 || len(f.PCM) != n {
		return fmt.Errorf("%w: stream %d with %d samples", ErrBadFrame, f.Stream, len(f.PCM))
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return ErrQueueClosed
	}
	if q.n == len(q.frames) {
		q.spare = append(q.spare, q.popFrame())
		q.dropped++
	}
	buf := q.takeSpare()
	if buf == nil {
		buf = make([]byte, 0, maxFrameBytes)
	}
	q.frames[(q.head+q.n)%len(q.frames)] = AppendFrame(buf[:0], f)
	q.n++
	q.signal()
	return nil
}

// Dropped returns how many frames the queue has dropped to stay in its bound.
func (q *SendQueue) Dropped() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.dropped
}

// Serve writes the queue's messages on w until ctx ends or a write fails,
// and returns why it ended. Then the queue is closed. A write in progress
// ends only when w returns, so the side closes its connection when ctx ends.
// Serve runs once per queue, so a link has one writer: a call while another
// Serve runs returns ErrQueueServing, and a call after it ended returns
// ErrQueueClosed.
func (q *SendQueue) Serve(ctx context.Context, w MessageWriter) error {
	q.mu.Lock()
	switch {
	case q.closed:
		q.mu.Unlock()
		return ErrQueueClosed
	case q.serving:
		q.mu.Unlock()
		return ErrQueueServing
	}
	q.serving = true
	q.mu.Unlock()
	err := q.serve(ctx, w)
	q.mu.Lock()
	q.closed = true
	q.hello, q.controls, q.frames, q.spare, q.n = nil, nil, nil, nil, 0
	q.mu.Unlock()
	return err
}

func (q *SendQueue) serve(ctx context.Context, w MessageWriter) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		kind, b := q.next()
		if b == nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-q.wake:
			}
			continue
		}
		if err := w.WriteMessage(kind, b); err != nil {
			return err
		}
		if kind == websocket.BinaryMessage {
			q.mu.Lock()
			q.spare = append(q.spare, b)
			q.mu.Unlock()
		}
	}
}

// next takes the message to write next: the hello, then the oldest control
// message, then the oldest frame. It returns nil when none can go yet.
func (q *SendQueue) next() (int, []byte) {
	q.mu.Lock()
	defer q.mu.Unlock()
	switch {
	case q.hello == nil:
		return 0, nil
	case !q.helloSent:
		q.helloSent = true
		return websocket.TextMessage, q.hello
	case len(q.controls) > 0:
		b := q.controls[0]
		q.controls[0] = nil
		q.controls = q.controls[1:]
		return websocket.TextMessage, b
	case q.n > 0:
		return websocket.BinaryMessage, q.popFrame()
	}
	return 0, nil
}

// popFrame takes the oldest frame from the ring; q.mu is held and q.n > 0.
func (q *SendQueue) popFrame() []byte {
	b := q.frames[q.head]
	q.frames[q.head] = nil
	q.head = (q.head + 1) % len(q.frames)
	q.n--
	return b
}

// takeSpare takes a frame buffer to reuse, or nil; q.mu is held.
func (q *SendQueue) takeSpare() []byte {
	k := len(q.spare)
	if k == 0 {
		return nil
	}
	b := q.spare[k-1]
	q.spare[k-1] = nil
	q.spare = q.spare[:k-1]
	return b
}

// signal tells Serve a message came; q.mu is held.
func (q *SendQueue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}
