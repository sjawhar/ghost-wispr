package concierge

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/dispatch"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// TestAStatusIsGivenInWords (live on 2026-11-25): the room heard "INFRA-14
// is in needs_review, updated 12 minutes ago.", a status as Dispatch writes
// it. It fails on main (c57adc3) unless read_issue and search_issues give
// the model each status in words: the issue's status, its events' text and a
// search hit's text, and unless an agent's answer the voice speaks says a
// status in words.
func TestAStatusIsGivenInWords(t *testing.T) {
	r := newRig(t)
	r.records.issues["INFRA-14"] = &dispatch.Issue{
		Key: "INFRA-14", Title: "Pull-request watcher", Status: "needs_review",
		UpdatedAt: testNow.Add(-12 * time.Minute),
	}
	r.records.events["INFRA-14"] = []dispatch.Event{{Type: "comment.created", CreatedAt: testNow.Add(-12 * time.Minute), Payload: json.RawMessage(`{"body":"Moved from in_progress to needs_review."}`)}}
	read := r.run(t, "read_issue", map[string]string{"key": "INFRA-14"})
	if read["status"] != "needs review" {
		t.Errorf("read_issue status = %q, want %q", read["status"], "needs review")
	}
	events, _ := read["recent_events"].([]any)
	if first, _ := events[0].(map[string]any); first["text"] != "Moved from in progress to needs review." {
		t.Errorf("read_issue event text = %q, want the statuses in words", first["text"])
	}

	r = newRig(t)
	r.records.hits = []dispatch.SearchHit{
		{Kind: "issue", IssueKey: "INFRA-14", Title: "Pull-request watcher", Status: "needs_review", Snippet: "now needs_review", Score: 0.9},
	}
	r.records.kwHits = slices.Clone(r.records.hits)
	hits := r.run(t, "search_issues", map[string]string{"query": "watcher"})
	issues, _ := hits["issues"].([]any)
	if len(issues) == 0 {
		t.Fatalf("search_issues returned no issues: %v", hits)
	}
	if first, _ := issues[0].(map[string]any); first["status"] != "needs review" || first["match"] != "now needs review" {
		t.Errorf("search_issues first issue = %v, want its status and its text in words", first)
	}

	r = newRig(t)
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("INFRA-14 is in needs review."))}}
	spoken := r.concierge.forEar(context.Background(), "INFRA-14 is in needs_review.", "INFRA-14")
	if prompt := promptOf(r.model.requests[0]); spoken != "INFRA-14 is in needs review." || strings.Contains(prompt, "needs_review") {
		t.Errorf("an agent's answer is spoken %q from a rewrite asked of %q; want %q from its words", spoken, prompt, "INFRA-14 is in needs review.")
	}

	// Controls: the voice's own status lines said the status in words on
	// main too, and still do.
	if line := (&reading{key: "INFRA-14", status: "needs_review", newest: testNow.Add(-12 * time.Minute)}).known(testNow); line != "INFRA-14 is needs review, updated 12 minutes ago." {
		t.Errorf("known = %q", line)
	}
	if line := (&reading{key: "INFRA-14", status: "in_progress", newest: testNow.Add(-24 * time.Hour)}).agedStatus(testNow); !strings.Contains(line, " was in progress as of ") {
		t.Errorf("agedStatus = %q", line)
	}
}

// TestTheModelReadsNoStatusToken (live on 2026-11-25): "INFRA-14 is in
// needs_review" came from the model, whose record held Dispatch's status as
// written. It fails on main unless the record the model answers from holds
// no status token, in the issue's status or in its events' text.
func TestTheModelReadsNoStatusToken(t *testing.T) {
	r := newRig(t)
	r.records.issues["INFRA-14"] = &dispatch.Issue{
		Key: "INFRA-14", Title: "Pull-request watcher", Status: "needs_review",
		UpdatedAt: testNow.Add(-12 * time.Minute),
	}
	r.records.events["INFRA-14"] = []dispatch.Event{{Type: "comment.created", CreatedAt: testNow.Add(-12 * time.Minute), Payload: json.RawMessage(`{"body":"Moved to needs_review."}`)}}
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("INFRA-14 needs review."))}}
	var spoken []said
	if err := r.concierge.Answer(context.Background(), &r.exchange, "what is the status of INFRA-14", recorder(&spoken)); errors.Is(err, voice.ErrUnreachable) {
		t.Fatalf("Answer = %v", err)
	}
	if len(r.model.requests) == 0 {
		t.Fatalf("the model was not called")
	}
	prompt := promptOf(r.model.requests[0])
	if !strings.Contains(prompt, "INFRA-14") || strings.Contains(prompt, "needs_review") {
		t.Errorf("the model's record holds %q, or no INFRA-14: %s", "needs_review", prompt)
	}
}

// promptOf is the text of the model's first turn.
func promptOf(contents []*genai.Content) string {
	var b strings.Builder
	for _, c := range contents {
		for _, p := range c.Parts {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}
