package concierge

import (
	"slices"
	"strings"
	"testing"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/dispatch"
)

// TestReview106R2HeardDoesNotSearchBareNumberWords: the read-ahead must not
// send a whole sentence to Dispatch when the turn has a bare number and
// extra words: at most maxSearchTerms keywords, never the full question.
func TestReview106R2HeardDoesNotSearchBareNumberWords(t *testing.T) {
	const turn = "ask him for 4592 confidentialword"
	t.Run("real", func(t *testing.T) {
		r := newRig(t)
		probe106Issue(r)
		r.concierge.searchSettle = 0
		r.concierge.Heard(t.Context(), &r.exchange, turn)
		eventually(t, "the search query arrived", func() bool {
			r.records.mu.Lock()
			defer r.records.mu.Unlock()
			return len(r.records.queries)+len(r.records.kwQueries) > 0
		})
		r.records.mu.Lock()
		queries := append(slices.Clone(r.records.queries), r.records.kwQueries...)
		r.records.mu.Unlock()
		assertKeywordContract(t, queries, turn)
	})
	t.Run("planted_break", func(t *testing.T) {
		ft := &fakeT{}
		assertKeywordContract(ft, []string{turn}, turn)
		if !ft.failed {
			t.Fatal("the planted break was not caught: the test cannot detect a whole-sentence query")
		}
	})
}

func assertKeywordContract(t interface {
	Helper()
	Errorf(string, ...any)
}, queries []string, turn string) {
	for _, query := range queries {
		words := strings.Fields(query)
		if len(words) > maxSearchTerms {
			t.Errorf("query has %d words (max %d): %q", len(words), maxSearchTerms, query)
		}
		if query == turn {
			t.Errorf("the whole sentence was sent as the query: %q", query)
		}
	}
}

type fakeT struct{ failed bool }

func (f *fakeT) Helper()               {}
func (f *fakeT) Errorf(string, ...any) { f.failed = true }

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
