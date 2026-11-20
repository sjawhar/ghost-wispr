package gptlive

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// waitFactCheck waits for a word-free fact discrepancy count.
func waitFactCheck(t *testing.T, s voice.Session) int {
	t.Helper()
	deadline := time.After(testWait)
	for {
		select {
		case ev, ok := <-s.Events():
			if !ok {
				t.Fatal("Events closed before the fact check")
			}
			if ev.Kind == voice.EventFactCheck {
				if ev.Text != "" {
					t.Fatalf("fact check carries words %q, want a count only", ev.Text)
				}
				return ev.Count
			}
		case <-deadline:
			t.Fatal("no fact check")
		}
	}
}

// GPT-Live can paraphrase a written fact. Count each changed issue key, pull
// request number and figure after speech without holding or changing the
// words the room hears.
func TestSpokenFactCheckCountsChangedKeysAndFigures(t *testing.T) {
	srv := newLiveServer(t)
	s, c, _ := openSession(t, srv, testExchange(newFakeOutput(), newFakeTools(voice.AnswerResult{})))

	done := make(chan error, 1)
	go func() {
		done <- s.Say(context.Background(), "ABC-123 is in review. PR 42 has 3.5% failures.", voice.Fact)
	}()
	c.expect(evCommentaryAppend)
	c.speak("ABC-124 is in review. PR 43 has 4.5% failures.")
	if err := <-done; err != nil {
		t.Fatalf("Say: %v", err)
	}
	if got := waitFactCheck(t, s); got != 6 {
		t.Fatalf("spoken fact errors = %d, want 6 for three changed facts", got)
	}
}

func TestSpokenFactCheckAcceptsUnchangedFact(t *testing.T) {
	const fact = "ABC-123 is in review. PR 42 has 3.5% failures."
	srv := newLiveServer(t)
	s, c, _ := openSession(t, srv, testExchange(newFakeOutput(), newFakeTools(voice.AnswerResult{})))

	done := make(chan error, 1)
	go func() { done <- s.Say(context.Background(), fact, voice.Fact) }()
	c.expect(evCommentaryAppend)
	c.speak(fact)
	if err := <-done; err != nil {
		t.Fatalf("Say: %v", err)
	}
	if got := waitFactCheck(t, s); got != 0 {
		t.Fatalf("spoken fact errors = %d, want 0 for unchanged facts", got)
	}
}

// A held answer is a delegation result, not Say. It gets the same count.
func TestSpokenFactCheckCoversHeldAnswer(t *testing.T) {
	srv := newLiveServer(t)
	s, c, _ := openSession(t, srv, testExchange(newFakeOutput(), newFakeTools(voice.AnswerResult{})))
	c.delegate("held")
	// The automatic tool answer is empty: close it before the held result.
	_ = c.expect(evCommentaryAppend)

	result := []byte(`{"say":"ABC-123 is in review. PR 42 has 3.5% failures."}`)
	done := make(chan error, 1)
	go func() { done <- s.Deliver(context.Background(), "held", result) }()
	c.expect(evCommentaryAppend)
	c.speak("ABC-999 is in review. PR 42 has 3.5% failures.")
	if err := <-done; err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if got := waitFactCheck(t, s); got != 2 {
		t.Fatalf("held answer errors = %d, want 2 for a changed issue key", got)
	}
}
