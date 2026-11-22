package concierge

import (
	"context"
	"regexp"
	"strconv"
	"testing"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/envoy"
	"github.com/sjawhar/ghost-wispr/internal/monoclock"
)

// TestAnAcceptedAnswerIsLoggedWithItsMonotonicTime: the line that records an
// agent's answer carries at_mono_ns, CLOCK_MONOTONIC read right after the
// store took the answer, on the clock the acceptance driver keeps its own
// times on. The ask-holder step reads it to tell an answer that came before
// "Wispr, never mind" ended from one its cancel should have stopped.
func TestAnAcceptedAnswerIsLoggedWithItsMonotonicTime(t *testing.T) {
	r := newRig(t)
	logs := r.captureLogs()
	r.ask(t, "agent-7", "is it merged", "ABC-12")
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("It merged."))}}
	before := monoclock.Read()
	if err := r.concierge.Deliver(context.Background(), &envoy.Message{EventID: "r", SourceSession: "agent-7", InReplyTo: "evt-1", Text: "It merged."}); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	after := monoclock.Read()
	match := regexp.MustCompile(`msg="concierge took an agent's answer" task=evt-1 session=meeting-1 at_mono_ns=(\d+)`).FindStringSubmatch(logs.String())
	if match == nil {
		t.Fatalf("no answer line with at_mono_ns:\n%s", logs)
	}
	at, err := strconv.ParseInt(match[1], 10, 64)
	if err != nil || at < before || at > after {
		t.Fatalf("at_mono_ns = %s, want a CLOCK_MONOTONIC reading within the delivery, %d to %d", match[1], before, after)
	}
}
