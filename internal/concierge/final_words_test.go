package concierge

import (
	"context"
	"slices"
	"testing"
	"time"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/dispatch"
)

// TestAnswerSearchesTheFinalQuestionTheReadAheadDidNot (R6; the proof of
// #187): the read-ahead searches the question said so far, and the final
// turn can add words ("what's one blocker on the meeting voice" was searched
// as "blocker" alone). The answer takes the read-ahead's search only when
// the final turn's question, as the full search is sent it, is the one the
// read-ahead searched. It fails if the answer takes a read-ahead whose
// question is not the final turn's, a word that names no work or the same
// words in another order included, or searches again when it is, case and
// punctuation aside.
func TestAnswerSearchesTheFinalQuestionTheReadAheadDidNot(t *testing.T) {
	const heard, searched = "what is happening with the realtime voice", "realtime voice"
	for _, tc := range []struct {
		name, turn    string
		full, keyword []string
	}{
		{"the final turn adds a word of work", "what is happening with the realtime voice rollout",
			[]string{heard, "what is happening with the realtime voice rollout"}, []string{searched, "realtime voice rollout"}},
		{"the final turn adds a word that names no work", "what is happening with the realtime voice today",
			[]string{heard, "what is happening with the realtime voice today"}, []string{searched, searched}},
		{"the same words in another order", "what is happening with the voice, realtime?",
			[]string{heard, "what is happening with the voice realtime"}, []string{searched, "voice realtime"}},
		{"control: the same question", "What is happening with the realtime voice?", []string{heard}, []string{searched}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			r.concierge.searchSettle = 50 * time.Millisecond
			unclearSearch(r)
			r.records.hits[0].Score = 0.012
			r.concierge.Heard(context.Background(), &r.exchange, heard)
			eventually(t, "the read-ahead searched", func() bool { return len(r.records.searches()) == 1 })
			r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("PR 80 is in review."))}}
			spoken := answerTurn(t, r, tc.turn)
			r.records.mu.Lock()
			kw := slices.Clone(r.records.kwQueries)
			r.records.mu.Unlock()
			if full := r.records.searches(); !slices.Equal(full, tc.full) || !slices.Equal(kw, tc.keyword) {
				t.Fatalf("full searches %q, keyword %q; want %q and %q", full, kw, tc.full, tc.keyword)
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

// TestAnEmptyKeywordFallbackWaitsForTheFullSearch (R5; Main's decision on
// #189): when no keyword issue's title holds a word searched, the voice waits
// for the full search, which reads the meaning, up to its own bound, and
// offers its result as it came, after the routing line; it says no issue was
// found only when the full search also fails or finds nothing. A kept keyword
// issue is still offered at once.
func TestAnEmptyKeywordFallbackWaitsForTheFullSearch(t *testing.T) {
	settings := dispatch.SearchHit{Kind: "document", IssueKey: "ABC-50", Title: "Provider settings page", Score: 0.02}
	gateway := dispatch.SearchHit{Kind: "document", IssueKey: "ABC-52", Title: "Model gateway sign-in", Score: 0.005}
	roadmap := dispatch.SearchHit{Kind: "document", IssueKey: "ABC-9", Title: "Roadmap: one grader", Score: 0.0007}
	budget := dispatch.SearchHit{Kind: "document", IssueKey: "ABC-1", Title: "Budget", Score: 0.0007}
	for _, tc := range []struct {
		name        string
		turn        string
		kw, full    []dispatch.SearchHit
		fullDelay   time.Duration
		fullWait    time.Duration
		searchBound time.Duration
		want        []string
		// tookAtLeast and tookAtMost bound the answer's time.
		tookAtLeast, tookAtMost time.Duration
	}{
		{
			name: "a clear keyword lead no title names, the full result clear at 4 s",
			turn: "what is happening with the kore voice",
			kw:   []dispatch.SearchHit{settings, gateway},
			full: []dispatch.SearchHit{
				{Kind: "document", IssueKey: "ABC-50", Title: "Provider settings page", Score: 0.01},
				{Kind: "document", IssueKey: "ABC-53", Title: "Speaker trace", Score: 0.002},
			},
			fullDelay:   4 * time.Second,
			want:        []string{"Reading the record.", "I took ABC-50."},
			tookAtLeast: 4 * time.Second,
		},
		{
			name: "keyword titles name none of the final words, the full result unclear at 4 s",
			turn: "what's one blocker on the meeting voice",
			kw:   []dispatch.SearchHit{roadmap, budget},
			full: []dispatch.SearchHit{
				{Kind: "comment", IssueKey: "ABC-60", Title: "Meeting voice measurements", Score: 0.0026},
				{Kind: "document", IssueKey: "ABC-61", Title: "Agents join a meeting", Score: 0.0025},
			},
			fullDelay:   4 * time.Second,
			want:        []string{"Reading the record.", "Did you mean A B C sixty, or A B C sixty-one?"},
			tookAtLeast: 4 * time.Second,
		},
		{
			name:        "the full search finds nothing either",
			turn:        "what's one blocker on the meeting voice",
			kw:          []dispatch.SearchHit{roadmap, budget},
			full:        []dispatch.SearchHit{},
			fullDelay:   3 * time.Second,
			want:        []string{"Reading the record.", noIssueFound},
			tookAtLeast: 3 * time.Second,
		},
		{
			name:        "the full search fails at its bound",
			turn:        "what's one blocker on the meeting voice",
			kw:          []dispatch.SearchHit{roadmap, budget},
			fullDelay:   time.Hour,
			searchBound: 3 * time.Second,
			want:        []string{"Reading the record.", noIssueFound},
			tookAtLeast: 3 * time.Second,
		},
		{
			name: "control: a kept keyword issue is offered at once",
			turn: "what is happening with the kore voice",
			kw: []dispatch.SearchHit{settings,
				{Kind: "issue", IssueKey: "ABC-51", Title: "Kore voice tuning", Score: 0.005},
			},
			full:       []dispatch.SearchHit{settings},
			fullDelay:  4 * time.Second,
			fullWait:   200 * time.Millisecond,
			want:       []string{"Did you mean A B C fifty-one?"},
			tookAtMost: time.Second,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			if tc.fullWait > 0 {
				r.concierge.fullWait = tc.fullWait
			}
			if tc.searchBound > 0 {
				r.concierge.searchBound = tc.searchBound
			}
			r.records.searchDelay = tc.fullDelay
			r.records.hits, r.records.kwHits = tc.full, tc.kw
			r.records.issues["ABC-50"] = &dispatch.Issue{Key: "ABC-50", Title: "Provider settings page", Status: "in_progress", UpdatedAt: testNow}
			r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("It is in progress."))}}
			start := time.Now()
			spoken := answerTurn(t, r, tc.turn)
			took := time.Since(start)
			var texts []string
			for _, s := range spoken {
				texts = append(texts, s.text)
			}
			if len(texts) < len(tc.want) || !slices.Equal(texts[:len(tc.want)], tc.want) {
				t.Fatalf("spoken %q, want it to begin %q", texts, tc.want)
			}
			if took < tc.tookAtLeast || (tc.tookAtMost > 0 && took > tc.tookAtMost) {
				t.Fatalf("the answer took %v, want at least %v and at most %v", took, tc.tookAtLeast, tc.tookAtMost)
			}
		})
	}
}
