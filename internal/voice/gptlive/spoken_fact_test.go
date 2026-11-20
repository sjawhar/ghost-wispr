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

// GPT-Live can paraphrase a written fact. Count the shared R8 ear-check
// verdict after speech without holding or changing the words the room hears.
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
	if got := waitFactCheck(t, s); got != 1 {
		t.Fatalf("spoken fact errors = %d, want one failed R8 verdict", got)
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
	if got := waitFactCheck(t, s); got != 1 {
		t.Fatalf("held answer errors = %d, want one failed R8 verdict", got)
	}
}

// The voice B count is Concierge's R8 result: names and form matter, while
// a figure read in words is the same figure.
func TestSpokenFactCheckUsesConciergeR8(t *testing.T) {
	for name, c := range map[string]struct {
		source string
		spoken string
		want   int
	}{
		"unchanged":      {"The owner is Alice. PR 3 is ready.", "The owner is Alice. PR 3 is ready.", 0},
		"name changed":   {"The owner is Alice. PR 3 is ready.", "The owner is Bob. PR 3 is ready.", 1},
		"number words":   {"PR 3 is ready.", "PR three is ready.", 0},
		"four sentences": {"PR 3 is ready.", "One. Two. Three. Four.", 1},
		"changed key":    {"ABC-123 is ready.", "ABC-999 is ready.", 1},
		"changed figure": {"PR 3 is ready.", "PR 4 is ready.", 1},
	} {
		t.Run(name, func(t *testing.T) {
			if got := spokenFactErrors(c.source, c.spoken, ""); got != c.want {
				t.Fatalf("spokenFactErrors(%q, %q) = %d, want %d", c.source, c.spoken, got, c.want)
			}
		})
	}
}

// The asker named ABC-123, so R8 requires GPT-Live to retain it even though
// it is also in the written source.
func TestSolR4FactAuditMustRetainTheQuestionsKey(t *testing.T) {
	for _, tc := range []struct {
		name, spoken string
		bad          bool
	}{
		{"unchanged key control", "ABC-123 is in review.", false},
		{"question key silently omitted", "It is in review.", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := testExchange(newFakeOutput(), newFakeTools(voice.AnswerResult{}))
			x.AskerTurn = "What is happening with ABC-123?"
			srv := newLiveServer(t)
			s, c, _ := openSession(t, srv, x)
			done := make(chan error, 1)
			go func() { done <- s.Say(context.Background(), "ABC-123 is in review.", voice.Fact) }()
			c.expect(evCommentaryAppend)
			c.speak(tc.spoken)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			n := waitFactCheck(t, s)
			if (n > 0) != tc.bad {
				t.Errorf("question explicitly named ABC-123; spoken=%q, fact failures=%d, expected invalid=%t", tc.spoken, n, tc.bad)
			}
		})
	}
}
