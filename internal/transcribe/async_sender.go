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
// chunk written before it.
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
	go s.run(ctx)
	return s
}

// Write queues a copy of p and never blocks.
func (s *AsyncSender) Write(p []byte) (int, error) {
	select {
	case i := <-s.free:
		s.slots[i] = append(s.slots[i][:0], p...)
		s.queue <- sendItem{slot: i}
	default:
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
			_, _ = s.dst.Write(s.slots[item.slot])
			if s.dropped.Swap(0) > 0 {
				s.log("live transcription caught up")
			}
			s.free <- item.slot
		}
	}
}
