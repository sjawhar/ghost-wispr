package transcribe

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// AsyncSender feeds audio to a live transcription client from a bounded
// queue, so the microphone loop never waits on the network: Write copies the
// chunk into a free slot and returns at once, and a goroutine writes the
// slots in order. With every slot taken, Write drops the chunk; the recording
// has it already. Finalize goes through the same queue, so it follows every
// chunk written before it. A ResilientClient behind it counts the audio
// queued here, and each chunk dropped here at its place, in its stream's
// clock (CapturedSince): that audio was captured after any the client has,
// though it took none of it.
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
	// queued counts the bytes in slots not yet counted by dst.
	queued atomic.Int64
	// watcher is dst when it counts this queue (ResilientClient), else nil.
	watcher sendQueueWatcher
	// readAt is when the microphone read the buffer the next Write takes
	// (MarkRead); hasRead says it is set. Guarded by readMu.
	readMu  sync.Mutex
	readAt  time.Time
	hasRead bool
}

// sendQueueWatcher is a destination that counts, in its stream's clock, the
// audio an AsyncSender in front of it holds and drops (ResilientClient).
type sendQueueWatcher interface {
	watchSendQueue(queued func() int64)
	sendQueueDropped(n int64)
	// writeFromQueue writes p, a chunk the queue holds, and takes it out of
	// queued as it counts it, in one step.
	writeFromQueue(p []byte, queued *atomic.Int64) (int, error)
	// captured stamps the n bytes the queue just took or dropped, the latest
	// audio counted, as captured by at.
	captured(at time.Time, n int64)
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
	if watcher, ok := dst.(sendQueueWatcher); ok {
		s.watcher = watcher
		watcher.watchSendQueue(s.queued.Load)
	}
	go s.run(ctx)
	return s
}

// MarkRead marks at as when the microphone read the buffer the next Write
// takes. The mic loop marks each buffer as it reads it, before checks that
// can hold it back, such as the read of the light; a Write with no mark
// takes its own time.
func (s *AsyncSender) MarkRead(at time.Time) {
	s.readMu.Lock()
	defer s.readMu.Unlock()
	s.readAt, s.hasRead = at, true
}

// Write queues a copy of p and never blocks. It stamps p for the destination
// as captured when the microphone read it (MarkRead), before it hands p on,
// so no result for p's words is timed from an earlier stamp
// (ResilientClient.CapturedAt).
func (s *AsyncSender) Write(p []byte) (int, error) {
	at := time.Now()
	s.readMu.Lock()
	if s.hasRead {
		at, s.hasRead = s.readAt, false
	}
	s.readMu.Unlock()
	n := int64(len(p))
	select {
	case i := <-s.free:
		s.slots[i] = append(s.slots[i][:0], p...)
		s.queued.Add(n)
		if s.watcher != nil {
			s.watcher.captured(at, n)
		}
		s.queue <- sendItem{slot: i}
	default:
		if s.watcher != nil {
			s.watcher.sendQueueDropped(n)
			s.watcher.captured(at, n)
		}
		if s.dropped.Add(1) == 1 {
			slog.Warn("live transcription is behind; dropping audio for it (the recording keeps it)")
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
			if s.watcher != nil {
				// The chunk leaves the queue's count as the client counts it,
				// in one step, so a capture resume that marks the count
				// (Captured) marks the pause neither early nor late.
				_, _ = s.watcher.writeFromQueue(s.slots[item.slot], &s.queued)
			} else {
				_, _ = s.dst.Write(s.slots[item.slot])
				s.queued.Add(-int64(len(s.slots[item.slot])))
			}
			if s.dropped.Swap(0) > 0 {
				s.log("live transcription caught up")
			}
			s.free <- item.slot
		}
	}
}
