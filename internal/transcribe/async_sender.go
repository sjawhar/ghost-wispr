package transcribe

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"time"
)

// AsyncSender feeds audio to a live transcription client from a bounded
// queue, so the microphone loop never waits on the network: Write copies the
// chunk into a free slot and returns at once, and a goroutine writes the
// slots in order. With every slot taken, Write drops the chunk; the recording
// has it already. Finalize goes through the same queue, so it follows every
// chunk written before it. A ResilientClient behind it counts the audio
// queued and dropped here in its stream's clock (CapturedSince): that audio
// was captured after any the client has, though it took none of it.
type AsyncSender struct {
	dst interface {
		io.Writer
		Finalize() error
	}
	slots   [][]byte
	free    chan int
	queue   chan sendItem
	dropped atomic.Int64
	log     func(string, ...any)
	// queued counts the bytes in slots not yet handed to dst, and skipped
	// the bytes Write ever dropped.
	queued  atomic.Int64
	skipped atomic.Int64
}

// backlogCounter is a destination that counts the audio an AsyncSender
// holds or dropped in front of it (ResilientClient).
type backlogCounter interface {
	countBacklog(backlog func() (queued, skipped int64))
}

type sendItem struct {
	slot int
	done chan error // a Finalize when not nil
}

// ErrSendQueueFull is Finalize's answer when its request does not fit.
var ErrSendQueueFull = errors.New("live transcription send queue is full")

// NewAsyncSender starts a sender of up to chunks queued chunks to dst; it
// stops when ctx ends.
func NewAsyncSender(ctx context.Context, dst interface {
	io.Writer
	Finalize() error
}, chunks int, logger func(string, ...any)) *AsyncSender {
	if logger == nil {
		logger = func(string, ...any) {}
	}
	s := &AsyncSender{
		dst:   dst,
		slots: make([][]byte, chunks),
		free:  make(chan int, chunks),
		queue: make(chan sendItem, chunks+8),
		log:   logger,
	}
	for i := range chunks {
		s.free <- i
	}
	if counter, ok := dst.(backlogCounter); ok {
		counter.countBacklog(s.backlog)
	}
	go s.run(ctx)
	return s
}

// backlog returns the bytes queued for dst now and the bytes ever dropped.
func (s *AsyncSender) backlog() (queued, skipped int64) {
	return s.queued.Load(), s.skipped.Load()
}

// Write queues a copy of p and never blocks.
func (s *AsyncSender) Write(p []byte) (int, error) {
	select {
	case i := <-s.free:
		s.slots[i] = append(s.slots[i][:0], p...)
		s.queued.Add(int64(len(p)))
		s.queue <- sendItem{slot: i}
	default:
		s.skipped.Add(int64(len(p)))
		if s.dropped.Add(1) == 1 {
			s.log("warning: live transcription is behind; dropping audio for it (the recording keeps it)")
		}
	}
	return len(p), nil
}

// Finalize asks dst to finalize once every chunk queued before it is sent.
// It waits up to finalizeQueueWait for its turn and the answer.
func (s *AsyncSender) Finalize() error {
	done := make(chan error, 1)
	select {
	case s.queue <- sendItem{slot: -1, done: done}:
	default:
		return ErrSendQueueFull
	}
	timer := time.NewTimer(finalizeQueueWait)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		return errors.New("live transcription finalize did not run in time")
	}
}

// finalizeQueueWait bounds Finalize's wait for the queue ahead of it.
const finalizeQueueWait = 10 * time.Second

func (s *AsyncSender) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case item := <-s.queue:
			if item.done != nil {
				item.done <- s.dst.Finalize()
				continue
			}
			// Out of the queue before dst counts it, so it is never counted
			// twice.
			s.queued.Add(-int64(len(s.slots[item.slot])))
			_, _ = s.dst.Write(s.slots[item.slot])
			if s.dropped.Swap(0) > 0 {
				s.log("live transcription caught up")
			}
			s.free <- item.slot
		}
	}
}
