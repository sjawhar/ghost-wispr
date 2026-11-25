package concierge

import (
	"slices"
	"testing"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/dispatch"
)

// TestReview106R2HeardSearchesBareNumberWords (Sami on 2026-11-24, in place
// of #106's rule that no search sends the sentence): a turn with a bare
// number and words beyond it is searched by the read-ahead: the full search
// gets the question, the bare number with it, and the keyword search its
// word that names work. It fails if either search gets other words.
func TestReview106R2HeardSearchesBareNumberWords(t *testing.T) {
	const turn = "ask him for 4592 confidentialword"
	r := newRig(t)
	probe106Issue(r)
	r.concierge.searchSettle = 0
	r.concierge.Heard(t.Context(), &r.exchange, turn)
	eventually(t, "both searches were sent", func() bool {
		r.records.mu.Lock()
		defer r.records.mu.Unlock()
		return len(r.records.queries) > 0 && len(r.records.kwQueries) > 0
	})
	r.records.mu.Lock()
	full, keyword := slices.Clone(r.records.queries), slices.Clone(r.records.kwQueries)
	r.records.mu.Unlock()
	if !slices.Equal(full, []string{turn}) || !slices.Equal(keyword, []string{"confidentialword"}) {
		t.Errorf("full %q, keyword %q; want %q and [confidentialword]", full, keyword, turn)
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
			want := "Did you mean A B C twelve, or A B C thirteen?"
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
	if len(spoken) != 1 || spoken[0].text != "Did you mean Infra forty-five ninety-two?" {
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
	if len(spoken) != 1 || spoken[0].text != "Did you mean Infra forty-five ninety-two?" {
		t.Fatalf("spoken=%+v want bare-number confirmation after empty search", spoken)
	}
}
