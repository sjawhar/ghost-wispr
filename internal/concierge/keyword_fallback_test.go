package concierge

import (
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/dispatch"
)

// TestTheKeywordFallbackOffersOnlyIssuesItsTitleNames (R5; the office Pi on
// 2026-11-24): when the full search misses its mark or fails, the keyword
// result is taken, and on the Pi it offered an issue whose own title holds
// none of the words said, found through a document about it, beside one the
// words name. It fails unless that fallback offers only the issues whose own
// title holds a word of the search, a stem's plural form included: two such
// issues are offered, one is offered alone, and none is no issue found. A
// full result in time is offered as it came.
func TestTheKeywordFallbackOffersOnlyIssuesItsTitleNames(t *testing.T) {
	var (
		contracts = dispatch.SearchHit{Kind: "issue", IssueKey: "ABC-41", Title: "Parser eval contracts", Score: 0.0104}
		stateFile = dispatch.SearchHit{Kind: "document", IssueKey: "ABC-7", Title: "Team state file", Score: 0.0102}
		answers   = dispatch.SearchHit{Kind: "issue", IssueKey: "ABC-42", Title: "Parser-evals: first answers", Score: 0.0102}
		roadmap   = dispatch.SearchHit{Kind: "issue", IssueKey: "ABC-9", Title: "Roadmap: one grader", Score: 0.0101}
	)
	for _, tc := range []struct {
		name string
		kw   []dispatch.SearchHit
		want string
	}{
		{"two titles hold the words", []dispatch.SearchHit{contracts, stateFile, answers, roadmap}, "Did you mean A B C forty-one, or A B C forty-two?"},
		{"one title holds them", []dispatch.SearchHit{contracts, stateFile, roadmap}, "Did you mean A B C forty-one?"},
		{"no title holds them", []dispatch.SearchHit{stateFile, roadmap}, noIssueFound},
		{"the only title that holds them is past the fifth issue", []dispatch.SearchHit{
			stateFile, roadmap,
			{Kind: "issue", IssueKey: "ABC-1", Title: "Budget", Score: 0.01},
			{Kind: "issue", IssueKey: "ABC-2", Title: "Hiring", Score: 0.0099},
			{Kind: "issue", IssueKey: "ABC-3", Title: "Lights", Score: 0.0098},
			{Kind: "issue", IssueKey: "ABC-41", Title: "Parser eval contracts", Score: 0.0097},
		}, "Did you mean A B C forty-one?"},
	} {
		for _, fallback := range []string{"the full search missed its mark", "the full search failed"} {
			t.Run(tc.name+", "+fallback, func(t *testing.T) {
				r := newRig(t)
				if fallback == "the full search failed" {
					r.concierge.searchBound = 200 * time.Millisecond
				} else {
					r.concierge.fullWait = 200 * time.Millisecond
				}
				r.records.searchDelay = time.Hour
				r.records.hits = []dispatch.SearchHit{stateFile, roadmap}
				r.records.kwHits = tc.kw
				spoken := answerTurn(t, r, "what is happening with the parser evals")
				if len(spoken) != 1 || spoken[0].text != tc.want {
					t.Fatalf("spoken = %+v, want only %q", spoken, tc.want)
				}
				if len(r.model.requests) != 0 {
					t.Fatalf("model calls %d, want none", len(r.model.requests))
				}
			})
		}
	}
	t.Run("control: a full result in time is offered as it came", func(t *testing.T) {
		r := newRig(t)
		r.records.searchDelay = 20 * time.Millisecond
		r.records.hits = []dispatch.SearchHit{stateFile, roadmap}
		r.records.kwHits = []dispatch.SearchHit{contracts, stateFile, answers, roadmap}
		spoken := answerTurn(t, r, "what is happening with the parser evals")
		if want := "Did you mean A B C seven, or A B C nine?"; len(spoken) != 1 || spoken[0].text != want {
			t.Fatalf("spoken = %+v, want only %q", spoken, want)
		}
	})
}

// TestAPickOfTheOneIssueOfferedAnswersTheQuestion (R5): a keyword fallback
// that offered one issue takes a yes, said as people say one, or the issue's
// key, as that issue, for the question first asked; a no as no pick; and
// anything else, such as side talk with "first" in it, as a question of its
// own. Live, "Yeah, that one." was searched as a question.
func TestAPickOfTheOneIssueOfferedAnswersTheQuestion(t *testing.T) {
	for reply, want := range map[string]struct {
		key string
		ok  bool
	}{
		"yes":                  {"ABC-41", true},
		"Yeah, that one.":      {"ABC-41", true},
		"Yep.":                 {"ABC-41", true},
		"Sure, that's it.":     {"ABC-41", true},
		"ABC-41, please":       {"ABC-41", true},
		"the first one":        {"", false},
		"no":                   {"", true},
		"Nope, not that one.":  {"", true},
		"what about the tests": {"", false},
		"yes and no":           {"", false},
	} {
		r := newRig(t)
		r.concierge.fullWait = 200 * time.Millisecond
		r.records.searchDelay = time.Hour
		r.records.kwHits = []dispatch.SearchHit{
			{Kind: "issue", IssueKey: "ABC-41", Title: "Parser eval contracts", Score: 0.0104},
			{Kind: "document", IssueKey: "ABC-7", Title: "Team state file", Score: 0.0102},
		}
		if spoken := answerTurn(t, r, "what is happening with the parser evals"); len(spoken) != 1 || spoken[0].text != "Did you mean A B C forty-one?" {
			t.Fatalf("spoken = %+v, want the one issue offered", spoken)
		}
		asked, key, ok := r.concierge.takePick(&r.exchange, reply)
		if key != want.key || ok != want.ok || (ok && asked != "what is happening with the parser evals") {
			t.Errorf("takePick(%q) = %q, %q, %v; want key %q, ok %v, for the question first asked", reply, asked, key, ok, want.key, want.ok)
		}
	}
}
