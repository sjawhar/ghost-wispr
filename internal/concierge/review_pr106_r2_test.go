package concierge

import (
	"slices"
	"strings"
	"testing"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/dispatch"
)

// TestReview106R2HeardDoesNotSearchBareNumberWords: the read-ahead must not
// send room words to Dispatch when the turn has a bare number and no key.
func TestReview106R2HeardDoesNotSearchBareNumberWords(t *testing.T) {
	r := newRig(t)
	probe106Issue(r)
	r.concierge.Heard(t.Context(), &r.exchange, "ask him for 4592 confidentialword")
	r.records.mu.Lock()
	queries := append(slices.Clone(r.records.queries), r.records.kwQueries...)
	r.records.mu.Unlock()
	for _, query := range queries {
		if strings.Contains(query, "confidentialword") {
			t.Fatalf("Heard sent room words to Dispatch: %q", queries)
		}
	}
}

// TestReview106R2DescriptionWithoutIssueAbout: "what is happening with
// realtime voice in room 4592?" has no "the issue about" phrasing but names
// real work. The keyword search must run and find ABC-12, not confirm
// INFRA-4592.
func TestReview106R2DescriptionWithoutIssueAbout(t *testing.T) {
	for _, clear := range []bool{true, false} {
		name := "clear"
		if !clear {
			name = "ambiguous"
		}
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			unclearSearch(r)
			probe106Issue(r)
			if clear {
				r.records.hits[0].Score = 0.012
			}
			r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("ABC-12 is in progress."))}}
			spoken := answerTurn(t, r, "what is happening with realtime voice in room 4592?")
			want := "Did you mean ABC-12, Realtime voice, or ABC-13, Wake word?"
			if clear {
				want = "ABC-12 is in progress."
			}
			if len(spoken) == 0 || spoken[0].text != want {
				t.Errorf("spoken=%+v want first=%q", spoken, want)
			}
		})
	}
}

// TestReview106R2NumberOnlyTurnConfirms: "4592" with no other words confirms
// without searching.
func TestReview106R2NumberOnlyTurnConfirms(t *testing.T) {
	r := newRig(t)
	probe106Issue(r)
	spoken := answerTurn(t, r, "4592")
	if len(spoken) != 1 || spoken[0].text != "Did you mean INFRA-4592?" {
		t.Fatalf("spoken=%+v want confirmation", spoken)
	}
	r.records.mu.Lock()
	queries := append(slices.Clone(r.records.queries), r.records.kwQueries...)
	r.records.mu.Unlock()
	if len(queries) != 0 {
		t.Fatalf("a number-only turn searched: %q", queries)
	}
}

// TestReview106R2WordsPlusNumberSearchesThenFallsBack: "ask him for 4592"
// has words beyond the number. The keyword search runs but finds nothing
// matching. The bare-number fallback then confirms.
func TestReview106R2WordsPlusNumberSearchesThenFallsBack(t *testing.T) {
	r := newRig(t)
	probe106Issue(r)
	r.records.issues["ABC-12"] = &dispatch.Issue{Key: "ABC-12", Title: "Realtime voice", Status: "in_progress", UpdatedAt: testNow}
	r.records.hits = []dispatch.SearchHit{} // search finds nothing
	spoken := answerTurn(t, r, "ask him for 4592")
	if len(spoken) != 1 || spoken[0].text != "Did you mean INFRA-4592?" {
		t.Fatalf("spoken=%+v want bare-number confirmation after empty search", spoken)
	}
}
