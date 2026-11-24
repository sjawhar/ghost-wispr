package concierge

import (
	"context"
	"slices"
	"testing"
	"time"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/dispatch"
)

// TestAnswerSearchesTheFinalWordsTheReadAheadDidNot (R6; the proof of #187):
// the read-ahead searches the words said so far, and the final turn can add
// words that name the work ("what's one blocker on the meeting voice" was
// searched as "blocker" alone). It fails if the answer takes a read-ahead
// whose words are not the final turn's words, or searches again when they
// are.
func TestAnswerSearchesTheFinalWordsTheReadAheadDidNot(t *testing.T) {
	for _, tc := range []struct {
		name, heard, turn string
		want              []string
	}{
		{"the final turn adds words", "what is happening with the realtime voice", "what is happening with the realtime voice rollout", []string{"realtime voice", "realtime voice rollout"}},
		{"control: the same words", "what is happening with the realtime voice", "what is happening with the realtime voice?", []string{"realtime voice"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			r.concierge.searchSettle = 50 * time.Millisecond
			unclearSearch(r)
			r.records.hits[0].Score = 0.012
			r.concierge.Heard(context.Background(), &r.exchange, tc.heard)
			eventually(t, "the read-ahead searched", func() bool { return len(r.records.searches()) == 1 })
			r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("PR 80 is in review."))}}
			spoken := answerTurn(t, r, tc.turn)
			r.records.mu.Lock()
			kw := slices.Clone(r.records.kwQueries)
			r.records.mu.Unlock()
			if full := r.records.searches(); !slices.Equal(full, tc.want) || !slices.Equal(kw, tc.want) {
				t.Fatalf("full searches %q, keyword %q; want %q", full, kw, tc.want)
			}
			if len(spoken) == 0 || spoken[0].text != "I took ABC-12." {
				t.Fatalf("spoken = %+v, want the issue the search chose", spoken)
			}
		})
	}
}

// TestAClearKeywordMatchMustBeNamedByItsTitle (R5; the proof of #187): a
// keyword hit names an issue through any of its texts, so a keyword result
// can lead clearly with an issue whose own title holds no word searched. It
// fails if such a lead is taken as one clear match rather than as no clear
// match, which the title filter of the keyword fallback then reads; or if a
// clear lead whose title holds a word searched is not taken at once.
func TestAClearKeywordMatchMustBeNamedByItsTitle(t *testing.T) {
	settings := dispatch.SearchHit{Kind: "document", IssueKey: "ABC-50", Title: "Provider settings page", Score: 0.02}
	tuning := dispatch.SearchHit{Kind: "issue", IssueKey: "ABC-51", Title: "Kore voice tuning", Score: 0.005}
	for _, tc := range []struct {
		name string
		kw   []dispatch.SearchHit
		want string
	}{
		{"the clear lead's title holds no word searched", []dispatch.SearchHit{settings, tuning}, "Did you mean A B C fifty-one?"},
		{"control: the clear lead's title holds them", []dispatch.SearchHit{
			{Kind: "issue", IssueKey: "ABC-51", Title: "Kore voice tuning", Score: 0.02},
			{Kind: "document", IssueKey: "ABC-50", Title: "Provider settings page", Score: 0.005},
		}, "I took ABC-51."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			r.concierge.fullWait = 200 * time.Millisecond
			r.records.searchDelay = time.Hour
			r.records.issues["ABC-51"] = &dispatch.Issue{Key: "ABC-51", Title: "Kore voice tuning", Status: "in_progress", UpdatedAt: testNow}
			r.records.kwHits = tc.kw
			r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("It is in progress."))}}
			spoken := answerTurn(t, r, "what is happening with the kore voice")
			if len(spoken) == 0 || spoken[0].text != tc.want {
				t.Fatalf("spoken = %+v, want %q first", spoken, tc.want)
			}
		})
	}
}
