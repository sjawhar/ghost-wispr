package session

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestWordsCapturedBeforeACloseReachNoLaterSession fails if a word captured
// before a session's recording closed reaches a later session's live
// transcript: its audio is in the ended session's recording, so deleting that
// session would leave the word in the next one. Deepgram can deliver it after
// the end is done, or it can wait in the buffer through the end.
func TestWordsCapturedBeforeACloseReachNoLaterSession(t *testing.T) {
	t.Run("delivered after the end", func(t *testing.T) {
		c := newCapture(t)
		c.manager.SetLiveStream(clockStream{c})
		c.say(1, 2)
		c.hear(timedResult(t, true, true, 0, "Bye."))
		first, _ := c.manager.ActiveSession()
		c.say(3) // "Secret.", 2-3 s into the stream: in the first session's recording
		if err := c.manager.endCurrentSession(context.Background()); err != nil {
			t.Fatal(err)
		}
		c.say(10, 11)
		c.hear(timedResult(t, true, true, 2*time.Second, "Secret.")) // Deepgram, lagging
		if next, _ := c.manager.ActiveSession(); next != "" {
			t.Fatalf("session %s, opened after %s ended, has the live transcript %q; it was captured before %s's recording closed", next, first, c.textOf(next), first)
		}
	})
	t.Run("buffered through the end", func(t *testing.T) {
		c := newCapture(t)
		c.manager.SetLiveStream(clockStream{c})
		c.say(1, 2)
		c.hear(timedResult(t, true, true, 0, "Bye."))
		c.say(3)
		c.hear(timedResult(t, true, false, 2*time.Second, "Secret.")) // a final, buffered until its speech ends
		if err := c.manager.endCurrentSession(context.Background()); err != nil {
			t.Fatal(err)
		}
		c.say(10, 11)
		c.hear(timedResult(t, true, true, 3*time.Second, "Hello", "again."))
		if got := c.currentText(); got != "Hello again." {
			t.Fatalf("the session opened after the end has the live transcript %q; want %q, nothing captured before the close", got, "Hello again.")
		}
	})
	t.Run("control: words after the close open the next session", func(t *testing.T) {
		c := newCapture(t)
		c.manager.SetLiveStream(clockStream{c})
		c.say(1, 2)
		c.hear(timedResult(t, true, true, 0, "Bye."))
		if err := c.manager.endCurrentSession(context.Background()); err != nil {
			t.Fatal(err)
		}
		c.say(10, 11)
		c.hear(timedResult(t, true, true, 2*time.Second, "Hello", "again."))
		if got := c.currentText(); got != "Hello again." {
			t.Fatalf("the session opened after the end has the live transcript %q; want %q", got, "Hello again.")
		}
	})
}

// TestMessageKeptOfAResultFromBeforeACloseIsNothing fails if MessageKept
// returns words captured before an ended session's recording closed, which
// the live transcript drops.
func TestMessageKeptOfAResultFromBeforeACloseIsNothing(t *testing.T) {
	c := newCapture(t)
	c.manager.SetLiveStream(clockStream{c})
	c.say(1, 2)
	c.hear(timedResult(t, true, true, 0, "Bye."))
	c.say(3)
	if err := c.manager.endCurrentSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	kept, err := c.manager.MessageKept(timedResult(t, true, true, 2*time.Second, "Secret."))
	if err != nil {
		t.Fatal(err)
	}
	if kept != nil {
		t.Fatalf("MessageKept returned %q, captured before the ended session's recording closed", strings.TrimSpace(kept.Channel.Alternatives[0].Transcript))
	}
}

// TestAManualStopKeepsItsBufferedWordsInItsLiveTranscript fails if words
// Deepgram finalized before a person stopped the session, still buffered for
// the end of their speech, are not in the stopped session's live transcript:
// their audio is in its recording, and the end drops them from the buffer, so
// only its refined transcript held them.
func TestAManualStopKeepsItsBufferedWordsInItsLiveTranscript(t *testing.T) {
	c := newCapture(t)
	c.manager.SetLiveStream(clockStream{c})
	c.say(1, 2)
	c.hear(timedResult(t, true, true, 0, "Bye."))
	stopped, _ := c.manager.ActiveSession()
	c.say(3)
	c.hear(timedResult(t, true, false, 2*time.Second, "Last.")) // a final, buffered until its speech ends
	if err := c.manager.StopSession(context.Background(), stopped); err != nil {
		t.Fatal(err)
	}
	if got, want := c.textOf(stopped), []string{"Bye.", "Last."}; !slices.Equal(got, want) {
		t.Fatalf("the stopped session's live transcript is %q; want %q, with the words buffered at the stop", got, want)
	}
}
