package session

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// reconnectingStream is a live stream with no lag whose current socket's
// stream starts origin into the capture: a word at into that stream was
// captured at origin plus at. A reconnect moves origin to where the next
// socket's stream starts.
type reconnectingStream struct {
	c      *capture
	mu     sync.Mutex
	origin time.Duration
}

func (s *reconnectingStream) reconnectAt(origin time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.origin = origin
}

func (s *reconnectingStream) start() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.origin
}

func (s *reconnectingStream) CapturedSince(at time.Duration) time.Duration {
	return max(s.c.said-s.start()-at, 0)
}

func (s *reconnectingStream) Captured() time.Duration { return s.c.said }

func (s *reconnectingStream) CapturedBefore(at time.Duration) time.Duration { return s.start() + at }

func (*reconnectingStream) MaxBehind() time.Duration { return time.Minute }

// TestAWordHeldAcrossAReconnectKeepsItsCaptureTime fails if a word the
// session holds in its buffer is placed, after a reconnect, by its time into
// the next socket's stream rather than the stream of the socket that heard
// it: "Last." was captured 2 s into the capture, before the first session's
// recording closed at 3 s, and the next socket's stream starts at 3 s, where
// the same stream time reads as 5 s. Read that way, it is after the close:
// it would go to the next session, whose recording does not hold its audio.
func TestAWordHeldAcrossAReconnectKeepsItsCaptureTime(t *testing.T) {
	t.Run("held through the end", func(t *testing.T) {
		c := newCapture(t)
		stream := &reconnectingStream{c: c}
		c.manager.SetLiveStream(stream)
		c.say(1, 2)
		c.hear(timedResult(t, true, true, 0, "Bye."))
		c.say(3)                                                    // "Last.", 2 s into the capture
		c.hear(timedResult(t, true, false, 2*time.Second, "Last.")) // a final, held until its speech ends
		stream.reconnectAt(3 * time.Second)
		if err := c.manager.endCurrentSession(context.Background()); err != nil {
			t.Fatal(err)
		}
		c.say(10, 11) // "Hello again.", 0 s into the next socket's stream
		c.hear(timedResult(t, true, true, 0, "Hello", "again."))
		if got := c.currentText(); got != "Hello again." {
			t.Fatalf("the session opened after the end has the live transcript %q; want %q, nothing captured before the close", got, "Hello again.")
		}
	})
	t.Run("held while the session ends", func(t *testing.T) {
		c, slow := endingCapture(t)
		stream := &reconnectingStream{c: c}
		c.manager.SetLiveStream(stream)
		c.say(1, 2)
		c.hear(timedResult(t, true, true, 0, "Bye."))
		first, _ := c.manager.ActiveSession()
		c.say(3)                                                    // "Last.", 2 s into the capture
		c.hear(timedResult(t, true, false, 2*time.Second, "Last.")) // a final, held until its speech ends
		stream.reconnectAt(3 * time.Second)
		ended := make(chan error, 1)
		go func() { ended <- c.manager.endCurrentSession(context.Background()) }()
		<-slow.closed
		c.say(10, 11) // "Good morning,", 0 s into the next socket's stream
		c.hear(timedResult(t, true, true, 0, "Good", "morning,"))
		close(slow.release)
		if err := <-ended; err != nil {
			t.Fatal(err)
		}
		next, _ := c.manager.ActiveSession()

		if got := c.textOf(first); !slices.Equal(got, []string{"Bye.", "Last."}) {
			t.Fatalf("the ending session's live transcript is %q; want %q: its recording holds both", got, []string{"Bye.", "Last."})
		}
		if next == "" || next == first {
			t.Fatalf("the current session is %q; want a next one, opened by the words spoken after the close", next)
		}
		if got := strings.Join(c.textOf(next), " "); got != "Good morning," {
			t.Fatalf("the next session's live transcript is %q; want %q", got, "Good morning,")
		}
	})
}
