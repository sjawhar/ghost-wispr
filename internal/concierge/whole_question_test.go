package concierge

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/dispatch"
)

// TestASearchSendsDispatchTheQuestion (Sami on 2026-11-24): a search sent
// Dispatch at most five words of the question. Live on 2026-11-24, the full
// search, words and meaning, put the issue meant first for 8 of 10
// questions when sent the whole question, against 4 of 10 for the five
// words; the keyword search found it for 5 of 10 with the words that name
// work, against 3 of 10 with the whole question. It fails if the full search
// is not sent the question as said, whole, in lower case and without the
// punctuation around its words, a spelled-out name as the word it spells,
// "real time" as "realtime", without the wake phrase it opens with and an
// issue key of Dispatch's projects; or if the keyword search is not sent
// each word that names work, however many.
func TestASearchSendsDispatchTheQuestion(t *testing.T) {
	for _, tc := range []struct{ query, full, keyword string }{
		{"has anyone fixed the xv6 eval build service that cannot reach the internet from builds linux?",
			"has anyone fixed the xv6 eval build service that cannot reach the internet from builds linux",
			"anyone fixed xv6 eval build service cannot reach internet builds linux"},
		{"What's happening with the ex v six evals?", "what's happening with the xv6 evals", "xv6 evals"},
		// A curly apostrophe is sent as a straight one.
		{"What’s happening with the ex v six evals?", "what's happening with the xv6 evals", "xv6 evals"},
		{"Wispr, what is happening with the realtime spoken conversation work?",
			"what is happening with the realtime spoken conversation work", "realtime spoken conversation"},
		{"Ghost Wispr, what about the ghost state of the xv6 proof?", "what about the ghost state of the xv6 proof", "state xv6 proof"},
		{"please ask the agent who holds the issue about realtime spoken conversation when slice one will be ready to try.",
			"please ask the agent who holds the issue about realtime spoken conversation when slice one will be ready to try",
			"realtime spoken conversation"},
		// ABC is a project of Dispatch's; LAST is none, so "the last 30
		// days" is speech, not a key.
		{"what is blocking ABC-99 on the realtime voice", "what is blocking on the realtime voice", "blocking realtime voice"},
		// The possessive goes with the key it follows, and leaves no "s".
		{"what is blocking ABC-99's realtime voice", "what is blocking realtime voice", "blocking realtime voice"},
		{"what is blocking ABC-99’s realtime voice", "what is blocking realtime voice", "blocking realtime voice"},
		{"what changed on the realtime voice in the last 30 days", "what changed on the realtime voice in the last 30 days", "realtime voice"},
		// A key as Deepgram writes one of a keyterm's project.
		{"what is blocking INFRA 4,592 on the realtime voice", "what is blocking on the realtime voice", "blocking realtime voice"},
		// "real time" is one word, as the keyword search reads it.
		{"what is happening with the real time voice", "what is happening with the realtime voice", "realtime voice"},
		// The wake phrase as Deepgram spells it, after an opener, goes; the
		// name inside the question stays.
		{"Whisper, what is happening with the realtime voice?", "what is happening with the realtime voice", "realtime voice"},
		{"Hey Wispr, what is happening with the realtime voice?", "what is happening with the realtime voice", "realtime voice"},
		{"what is blocking the ghost wispr rollout", "what is blocking the ghost wispr rollout", "blocking rollout"},
		{`what about the "parser evals"?`, "what about the parser evals", "parser evals"},
	} {
		r := newRig(t)
		r.run(t, "search_issues", map[string]string{"query": tc.query})
		r.records.mu.Lock()
		full, keyword := slices.Clone(r.records.queries), slices.Clone(r.records.kwQueries)
		r.records.mu.Unlock()
		if !slices.Equal(full, []string{tc.full}) || !slices.Equal(keyword, []string{tc.keyword}) {
			t.Errorf("search_issues(%q): full %q, keyword %q; want %q and %q", tc.query, full, keyword, tc.full, tc.keyword)
		}
	}
}

// TestAQuestionThatNamesNoWorkIsNotSearched: a question none of whose words
// names work reaches neither of Dispatch's searches. It fails if such a
// question is searched, or the model is not told to say what to search.
func TestAQuestionThatNamesNoWorkIsNotSearched(t *testing.T) {
	r := newRig(t)
	result := r.run(t, "search_issues", map[string]string{"query": "what is it, then?"})
	r.records.mu.Lock()
	searched := len(r.records.queries) + len(r.records.kwQueries)
	r.records.mu.Unlock()
	if result["error"] == nil || searched != 0 {
		t.Fatalf("a question with no word of work was searched: %v, %d searches", result, searched)
	}
}

// TestTheReadTargetIsTheQuestion: what a turn asks the record for carries
// what each search is sent, the question and the words that name work, so a
// read-ahead is reused only for the same two. A curly apostrophe reads as a
// straight one: a voice's model may write the final turn with one, Deepgram
// the read-ahead's without. It fails if a target leaves out either text, or
// two turns that send different words read as one target.
func TestTheReadTargetIsTheQuestion(t *testing.T) {
	r := newRig(t)
	for in, want := range map[string]string{
		"what's happening with X V six evals?":                       "search:what's happening with xv6 evals|xv6 evals",
		"what’s happening with X V six evals?":                       "search:what's happening with xv6 evals|xv6 evals",
		"What is happening with ABC-12, the realtime voice?":         "keys:ABC-12|what is happening with the realtime voice|realtime voice",
		"what is blocking ABC-12's realtime voice":                   "keys:ABC-12|what is blocking realtime voice|blocking realtime voice",
		"what is blocking ABC-12’s realtime voice":                   "keys:ABC-12|what is blocking realtime voice|blocking realtime voice",
		"what is happening with ABC-12":                              "keys:ABC-12||",
		"tell me about the issue about realtime voice, is it close?": "search:tell me about the issue about realtime voice is it close|realtime voice",
		"tell me about the issue about realtime voice is it close":   "search:tell me about the issue about realtime voice is it close|realtime voice close",
		"what is it, then?":                                          "",
	} {
		if got := r.concierge.readTarget(r.concierge.viewProjects(), readKeys(t, r, in)); got != want {
			t.Errorf("readTarget(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestAReadSearchesWhatTheToolSearches (found in review on 2026-11-24): a
// read of the record searched the question its target holds, which has lost
// its punctuation, so search_issues read it a second time. The keyword
// search then lost where a comma ended a description ("realtime voice
// close"), and the full search was not sent the question the target holds
// ("X. V. 6" went as "xv6"). It fails if a read sends either search other
// than what search_issues sends for the turn.
func TestAReadSearchesWhatTheToolSearches(t *testing.T) {
	for _, tc := range []struct{ turn, keyword string }{
		{"tell me about the issue about realtime voice, is it close to done?", "realtime voice"},
		{"What's the X. V. 6 eval?", "eval"},
	} {
		sent := func(read bool) (full, keyword []string) {
			r := newRig(t)
			if read {
				r.concierge.readFor(context.Background(), &r.exchange, r.concierge.viewProjects(), tc.turn, nil, nil)
			} else {
				r.run(t, "search_issues", map[string]string{"query": tc.turn})
			}
			r.records.mu.Lock()
			defer r.records.mu.Unlock()
			return slices.Clone(r.records.queries), slices.Clone(r.records.kwQueries)
		}
		readFull, readKeyword := sent(true)
		toolFull, toolKeyword := sent(false)
		if len(toolFull) != 1 || !slices.Equal(readFull, toolFull) || !slices.Equal(toolKeyword, []string{tc.keyword}) || !slices.Equal(readKeyword, toolKeyword) {
			t.Errorf("%q: a read searched full %q, keyword %q; search_issues full %q, keyword %q; want the same, keyword [%s]",
				tc.turn, readFull, readKeyword, toolFull, toolKeyword, tc.keyword)
		}
	}
}

// TestTheRecordHoldsNoFigureOrKeyTheAskerSaid (R8; found in review on
// 2026-11-24): the record a read makes is the source R8's check holds an
// answer to, and a figure or key the asker said is no fact of it
// (earCheckAsked). With the question in the line of its search, "what
// changed in the last 45 days" let "45" pass as the record's. It fails if a
// read's record holds the question's figure or its key that named no issue;
// the control shows the same sentence passes once the record holds it.
func TestTheRecordHoldsNoFigureOrKeyTheAskerSaid(t *testing.T) {
	for _, tc := range []struct{ turn, sentence, held string }{
		{"what changed on the realtime voice in the last 45 days", "It changed 45 times.", "45"},
		{"what is blocking ABC-99 on the realtime voice", "ABC-99 is blocked.", "ABC-99"},
	} {
		r := newRig(t)
		found := r.concierge.readFor(context.Background(), &r.exchange, r.concierge.viewProjects(), tc.turn, nil, nil)
		if !found.searched || found.text == "" {
			t.Fatalf("%q: the read made no search: %+v", tc.turn, found)
		}
		if earCheckAsked(tc.sentence, found.text+"\n"+tc.held, tc.turn, "") != "" {
			t.Fatalf("control: %q failed R8 with %q in the record", tc.sentence, tc.held)
		}
		if earCheckAsked(tc.sentence, found.text, tc.turn, "") == "" {
			t.Errorf("%q: %q passed R8 on the record %q", tc.turn, tc.sentence, found.text)
		}
	}
}

// TestKeyShapedWordsAreNotSentWhenTheListCannotTell (found in review on
// 2026-11-24): a key of one of Dispatch's projects is left out of the
// question, and a key in it put the issue meant second (rank 2 against 1,
// live on 2026-11-24). While the project list cannot tell (never read, or
// its read failed), any word that reads as a key may be one. It fails if
// such a word reaches the full search then, or if one of no project is left
// out once the list can tell.
func TestKeyShapedWordsAreNotSentWhenTheListCannotTell(t *testing.T) {
	unsure := projectsView{}
	known := projectsView{known: map[string]bool{"ABC": true}, sure: true}
	for _, tc := range []struct {
		view       projectsView
		turn, want string
	}{
		{unsure, "what is blocking ABC-99 on the realtime voice", "what is blocking on the realtime voice"},
		{unsure, "what changed on the realtime voice in the last 30 days", "what changed on the realtime voice in the days"},
		{known, "what is blocking ABC-99 on the realtime voice", "what is blocking on the realtime voice"},
		{known, "what changed on the realtime voice in the last 30 days", "what changed on the realtime voice in the last 30 days"},
	} {
		if got := tc.view.searchQuestion(tc.turn); got != tc.want {
			t.Errorf("sure=%v: searchQuestion(%q) = %q, want %q", tc.view.sure, tc.turn, got, tc.want)
		}
	}
}

// TestASearchReadsItsQuestionWithTheTurnsView (#190's rule): a turn decides
// from one reading of the project list, the search's question too. It fails
// if the read's search or the model's reads the list as it is when it
// searches, here after a read brought in the project LAST, rather than the
// turn's view; the control shows a runner with no turn reads the list then.
func TestASearchReadsItsQuestionWithTheTurnsView(t *testing.T) {
	const turn = "what changed on the realtime voice in the last 30 days"
	addLast := func(r *rig) {
		r.concierge.mu.Lock()
		defer r.concierge.mu.Unlock()
		projects := map[string]bool{"LAST": true}
		for project := range r.concierge.projects {
			projects[project] = true
		}
		r.concierge.projects, r.concierge.projectsRead = projects, testNow
	}
	fullSearches := func(r *rig) []string {
		r.records.mu.Lock()
		defer r.records.mu.Unlock()
		return slices.Clone(r.records.queries)
	}
	t.Run("a read", func(t *testing.T) {
		r := newRig(t)
		voiceIssue(r)
		view := r.concierge.viewProjects()
		addLast(r)
		r.concierge.readFor(context.Background(), &r.exchange, view, turn, nil, nil)
		if got := fullSearches(r); !slices.Equal(got, []string{turn}) {
			t.Errorf("full searches %q, want [%q]", got, turn)
		}
	})
	t.Run("the model's search", func(t *testing.T) {
		r := newRig(t)
		voiceIssue(r)
		r.model.onCall = func(call int) {
			if call == 0 {
				addLast(r)
			}
		}
		r.model.turns = [][]*genai.GenerateContentResponse{
			{chunk(call("a", "search_issues", map[string]any{"query": turn}, ""))},
			{chunk(text("ABC-12 is in progress."))},
		}
		answerTurn(t, r, turn)
		if got := fullSearches(r); !slices.Equal(got, []string{turn, turn}) {
			t.Errorf("full searches %q, want the read's and the model's, both [%q]", got, turn)
		}
	})
	t.Run("control: a runner with no turn", func(t *testing.T) {
		r := newRig(t)
		addLast(r)
		r.run(t, "search_issues", map[string]string{"query": turn})
		if got, want := fullSearches(r), []string{"what changed on the realtime voice in the days"}; !slices.Equal(got, want) {
			t.Errorf("full searches %q, want %q", got, want)
		}
	})
}

// TestAReadAheadIsReusedOnlyForTheSameKeywordWords: a read-ahead searched
// both texts its target holds. It fails if the answer takes a read-ahead
// whose question is the final turn's but whose keyword words are not: a
// comma the final turn adds ends the description the keyword search reads.
func TestAReadAheadIsReusedOnlyForTheSameKeywordWords(t *testing.T) {
	r := newRig(t)
	r.concierge.searchSettle = 50 * time.Millisecond
	unclearSearch(r)
	r.records.hits[0].Score = 0.012
	r.concierge.Heard(context.Background(), &r.exchange, "tell me about the issue about realtime voice is it close")
	eventually(t, "the read-ahead searched", func() bool { return len(r.records.searches()) == 1 })
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("PR 80 is in review."))}}
	answerTurn(t, r, "tell me about the issue about realtime voice, is it close?")
	r.records.mu.Lock()
	kw := slices.Clone(r.records.kwQueries)
	r.records.mu.Unlock()
	if want := []string{"realtime voice close", "realtime voice"}; !slices.Equal(kw, want) {
		t.Fatalf("keyword searches %q, want %q", kw, want)
	}
}

// TestAKeyReadThatNamedTheIssueIsReused: the question a key read holds is
// searched only when no key names an issue. It fails if, once a key read
// named its issue, a turn that says more words reads the key again, ahead or
// in the answer; the control shows a key that named no issue is searched for
// the final turn's question.
func TestAKeyReadThatNamedTheIssueIsReused(t *testing.T) {
	keyReads := func(r *rig, key string) int {
		return len(slices.DeleteFunc(r.records.calls(), func(k string) bool { return k != key }))
	}
	t.Run("a key that names an issue", func(t *testing.T) {
		r := newRig(t)
		voiceIssue(r)
		r.concierge.Heard(context.Background(), &r.exchange, "what about ABC-12 on the realtime voice")
		eventually(t, "the read of ABC-12", func() bool { return keyReads(r, "ABC-12") >= 2 })
		time.Sleep(100 * time.Millisecond)
		r.concierge.Heard(context.Background(), &r.exchange, "what about ABC-12 on the realtime voice rollout")
		time.Sleep(100 * time.Millisecond)
		r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("ABC-12 is in progress."))}}
		answerTurn(t, r, "what about ABC-12 on the realtime voice rollout today")
		if reads, searched := keyReads(r, "ABC-12"), len(r.records.searches()); reads > 2 || searched != 0 {
			t.Errorf("ABC-12 read %d times, %d searches; want the first read alone", reads, searched)
		}
	})
	t.Run("control: a key that names no issue", func(t *testing.T) {
		r := newRig(t)
		voiceIssue(r)
		r.concierge.Heard(context.Background(), &r.exchange, "what about ABC-99 on the realtime voice")
		eventually(t, "the read of ABC-99", func() bool { return keyReads(r, "ABC-99") >= 1 })
		time.Sleep(100 * time.Millisecond)
		r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("ABC-12 is in progress."))}}
		answerTurn(t, r, "what about ABC-99 on the realtime voice rollout")
		if got := r.records.searches(); !slices.Contains(got, "what about on the realtime voice rollout") {
			t.Errorf("full searches %q, want the final turn's question", got)
		}
	})
}

// TestTheTitleCheckReadsTheKeywordWords (R5): a clear keyword lead counts
// only when its own title holds a word the keyword search was sent. It fails
// if the check reads the question instead, which here holds "provider
// settings" after the description ends: the voice then took ABC-50 at once.
func TestTheTitleCheckReadsTheKeywordWords(t *testing.T) {
	r := newRig(t)
	r.concierge.fullWait = 200 * time.Millisecond
	r.records.searchDelay = time.Hour
	r.records.issues["ABC-50"] = &dispatch.Issue{Key: "ABC-50", Title: "Provider settings page", Status: "in_progress", UpdatedAt: testNow}
	r.records.issues["ABC-51"] = &dispatch.Issue{Key: "ABC-51", Title: "Kore voice tuning", Status: "in_progress", UpdatedAt: testNow}
	r.records.kwHits = []dispatch.SearchHit{
		{Kind: "document", IssueKey: "ABC-50", Title: "Provider settings page", Score: 0.02},
		{Kind: "issue", IssueKey: "ABC-51", Title: "Kore voice tuning", Score: 0.005},
	}
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("It is in progress."))}}
	spoken := answerTurn(t, r, "what is happening with the issue about the kore voice, the one on the provider settings page?")
	if len(spoken) == 0 || spoken[0].text != "Did you mean A B C fifty-one?" {
		t.Fatalf("spoken = %+v, want the issue the keyword words name offered", spoken)
	}
}

// TestASearchAgainSaysWhetherTheKeywordWordsWereTheSame: the log of an
// answer that searches again says whether its words that name work were the
// read-ahead's in any order, which the rule before reused for, so the unit's
// journal shows how often words of no work, or their order, cost a search.
// It fails if the line says otherwise, or names a word.
func TestASearchAgainSaysWhetherTheKeywordWordsWereTheSame(t *testing.T) {
	for _, tc := range []struct{ turn, want string }{
		{"what is happening with the realtime voice today", "same_keyword_words=true"},
		{"what is happening with the voice, realtime?", "same_keyword_words=true"},
		{"what is happening with the realtime voice rollout", "same_keyword_words=false"},
	} {
		r := newRig(t)
		logs := r.captureLogs()
		r.concierge.searchSettle = 50 * time.Millisecond
		unclearSearch(r)
		r.records.hits[0].Score = 0.012
		r.concierge.Heard(context.Background(), &r.exchange, "what is happening with the realtime voice")
		eventually(t, "the read-ahead searched", func() bool { return len(r.records.searches()) == 1 })
		r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("PR 80 is in review."))}}
		answerTurn(t, r, tc.turn)
		got := logs.String()
		if !strings.Contains(got, "concierge searches again") || !strings.Contains(got, tc.want) || strings.Contains(got, "realtime") {
			t.Errorf("%q: want %s in the log:\n%s", tc.turn, tc.want, got)
		}
	}
}

// TestAVendorsWordingTakesTheUnitsReadAhead (found in review on 2026-11-24):
// voices B and C answer a question in their vendor's words ("What's
// happening with the real-time voice?"), while the read-ahead searched the
// unit's own transcript of the same turn, the exchange's AskerTurn. It fails
// if such an answer searches again rather than take that read-ahead; the
// control shows a read-ahead of other words is not taken.
func TestAVendorsWordingTakesTheUnitsReadAhead(t *testing.T) {
	const unit = "what is happening with the realtime voice"
	for _, tc := range []struct {
		name, asker string
		searches    int
	}{
		{"the unit's words of the turn", unit, 1},
		{"control: the read-ahead heard other words", "what is happening with the wake word", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			r.concierge.searchSettle = 50 * time.Millisecond
			unclearSearch(r)
			r.records.hits[0].Score = 0.012
			r.exchange.AskerTurn = tc.asker
			r.concierge.Heard(context.Background(), &r.exchange, unit)
			eventually(t, "the read-ahead searched", func() bool { return len(r.records.searches()) == 1 })
			r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("PR 80 is in review."))}}
			answerTurn(t, r, "What's happening with the real-time voice?")
			r.records.mu.Lock()
			full, kw := len(r.records.queries), len(r.records.kwQueries)
			r.records.mu.Unlock()
			if full != tc.searches || kw != tc.searches {
				t.Errorf("full searches %d, keyword %d; want %d of each", full, kw, tc.searches)
			}
		})
	}
}

// TestAKeyReadIsNotStartedAgainWhileItRuns (found in review on 2026-11-24):
// a key's read holds the turn's search, so each word changed its target;
// each word then cancelled the key's read under way and read the key again.
// It fails if words said while a key is read read it again, or the answer
// does once that read named the issue.
func TestAKeyReadIsNotStartedAgainWhileItRuns(t *testing.T) {
	r := newRig(t)
	voiceIssue(r)
	release := make(chan struct{})
	r.records.mu.Lock()
	r.records.block, r.records.blockKey = release, "ABC-12"
	r.records.mu.Unlock()
	reads := func() int {
		return len(slices.DeleteFunc(r.records.calls(), func(k string) bool { return k != "ABC-12" }))
	}
	r.concierge.Heard(context.Background(), &r.exchange, "what about ABC-12 on the realtime voice")
	eventually(t, "the read of ABC-12 and its holder", func() bool { return reads() == 2 })
	r.concierge.Heard(context.Background(), &r.exchange, "what about ABC-12 on the realtime voice rollout")
	r.concierge.Heard(context.Background(), &r.exchange, "what about ABC-12 on the realtime voice rollout today")
	time.Sleep(100 * time.Millisecond)
	got := reads()
	close(release)
	if got != 2 {
		t.Fatalf("ABC-12 read %d times while the words came, want the first read's 2", got)
	}
	time.Sleep(100 * time.Millisecond)
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("ABC-12 is in progress."))}}
	answerTurn(t, r, "what about ABC-12 on the realtime voice rollout today")
	if got, searched := reads(), len(r.records.searches()); got != 2 || searched != 0 {
		t.Errorf("ABC-12 read %d times, %d searches; want the read-ahead's read alone", got, searched)
	}
}

// TestAKeyThatNamesNoIssueIsReadOnce (found in review on 2026-11-24): the
// read ahead of a key reads the key alone, once; once it named no issue, the
// turn's later words are its search, which waits for its words to settle and
// counts as the read-ahead's one (R6: two searches in all with the answer's).
// It fails if the key is read again for each word or in the answer, if the
// read-ahead searches more than once, or if the answer does not search the
// final turn's question.
func TestAKeyThatNamesNoIssueIsReadOnce(t *testing.T) {
	r := newRig(t)
	voiceIssue(r)
	r.concierge.searchSettle = 50 * time.Millisecond
	reads := func() int {
		return len(slices.DeleteFunc(r.records.calls(), func(k string) bool { return k != "ABC-99" }))
	}
	r.concierge.Heard(context.Background(), &r.exchange, "what about ABC-99 on the realtime voice")
	eventually(t, "the key's read", func() bool { return reads() >= 1 })
	time.Sleep(100 * time.Millisecond)
	first := reads()
	if searched := r.records.searches(); len(searched) != 0 {
		t.Fatalf("the key's read searched %q; want the key read alone", searched)
	}
	r.concierge.Heard(context.Background(), &r.exchange, "what about ABC-99 on the realtime voice rollout")
	eventually(t, "the later words searched", func() bool { return len(r.records.searches()) == 1 })
	r.concierge.Heard(context.Background(), &r.exchange, "what about ABC-99 on the realtime voice rollout today")
	time.Sleep(150 * time.Millisecond)
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("ABC-12 is in progress."))}}
	answerTurn(t, r, "what about ABC-99 on the realtime voice rollout today")
	searches := r.records.searches()
	if got := reads(); got != first || len(searches) != 2 || searches[1] != "what about on the realtime voice rollout today" {
		t.Errorf("ABC-99 read %d times (first read %d), full searches %q; want no more reads and two searches, the final question last", got, first, searches)
	}
}

// TestAFailedKeyReadIsReadAgain (found in review on 2026-11-24): a key's read
// that Dispatch did not answer found no issue only because it failed. The
// answer reads the key again; the read-ahead does not, for each word, while
// Dispatch fails. It fails if the answer searches in place of reading the
// key again, or later words read it ahead again.
func TestAFailedKeyReadIsReadAgain(t *testing.T) {
	r := newRig(t)
	voiceIssue(r)
	r.records.mu.Lock()
	r.records.err = fmt.Errorf("%w: dial tcp: connection refused", dispatch.ErrUnreachable)
	r.records.mu.Unlock()
	reads := func() int {
		return len(slices.DeleteFunc(r.records.calls(), func(k string) bool { return k != "ABC-12" }))
	}
	r.concierge.Heard(context.Background(), &r.exchange, "what about ABC-12 on the realtime voice")
	eventually(t, "the failed read of ABC-12", func() bool { return reads() >= 1 })
	time.Sleep(100 * time.Millisecond)
	failed := reads()
	r.concierge.Heard(context.Background(), &r.exchange, "what about ABC-12 on the realtime voice")
	r.concierge.Heard(context.Background(), &r.exchange, "what about ABC-12 on the realtime voice today")
	time.Sleep(100 * time.Millisecond)
	if got := reads(); got != failed {
		t.Fatalf("ABC-12 read %d times ahead after its read failed (%d before); want no more", got, failed)
	}
	r.records.mu.Lock()
	r.records.err = nil
	r.records.mu.Unlock()
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("ABC-12 is in progress."))}}
	spoken := answerTurn(t, r, "what about ABC-12 on the realtime voice today")
	if got, searched := reads(), r.records.searches(); got <= failed || len(searched) != 0 || len(spoken) == 0 || !strings.HasPrefix(spoken[0].text, "ABC-12") {
		t.Errorf("ABC-12 read %d times (%d before), searches %q, spoken %+v; want the key read again and answered, no search", got, failed, searched, spoken)
	}
}

// TestAnAnswerWaitsForTheKeyReadUnderWay (found in review on 2026-11-24): a
// final turn that comes while the read ahead of its key still runs waits for
// that read. It fails if the answer reads the key again, or searches.
func TestAnAnswerWaitsForTheKeyReadUnderWay(t *testing.T) {
	r := newRig(t)
	voiceIssue(r)
	release := make(chan struct{})
	r.records.mu.Lock()
	r.records.block, r.records.blockKey = release, "ABC-12"
	r.records.mu.Unlock()
	reads := func() int {
		return len(slices.DeleteFunc(r.records.calls(), func(k string) bool { return k != "ABC-12" }))
	}
	r.concierge.Heard(context.Background(), &r.exchange, "what about ABC-12 on the realtime voice")
	eventually(t, "the read of ABC-12 and its holder", func() bool { return reads() == 2 })
	go func() {
		time.Sleep(200 * time.Millisecond)
		close(release)
	}()
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("ABC-12 is in progress."))}}
	answerTurn(t, r, "what about ABC-12 on the realtime voice rollout today")
	if got, searched := reads(), len(r.records.searches()); got != 2 || searched != 0 {
		t.Errorf("ABC-12 read %d times, %d searches; want the read-ahead's read alone", got, searched)
	}
}

// TestAVendorsWordingTakesTheUnitsKeyRead (found in review on 2026-11-24):
// the unit's own transcript of the turn named a key whose read named its
// issue; a vendor's wording that leaves the key out takes that read. It fails
// if the answer searches or reads the key again.
func TestAVendorsWordingTakesTheUnitsKeyRead(t *testing.T) {
	const unit = "what about ABC-12 on the realtime voice"
	r := newRig(t)
	voiceIssue(r)
	r.exchange.AskerTurn = unit
	reads := func() int {
		return len(slices.DeleteFunc(r.records.calls(), func(k string) bool { return k != "ABC-12" }))
	}
	r.concierge.Heard(context.Background(), &r.exchange, "what about ABC-12")
	eventually(t, "the read of ABC-12", func() bool { return reads() >= 2 })
	time.Sleep(100 * time.Millisecond)
	r.concierge.Heard(context.Background(), &r.exchange, unit)
	time.Sleep(50 * time.Millisecond)
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("ABC-12 is in progress."))}}
	answerTurn(t, r, "What's happening with the real-time voice?")
	if got, searched := reads(), len(r.records.searches()); got != 2 || searched != 0 {
		t.Errorf("ABC-12 read %d times, %d searches; want the read-ahead's read alone", got, searched)
	}
}

// TestAVendorsKeysAreReadOverTheUnits (found in review on 2026-11-24): the
// unit heard one key, whose read named its issue, and the vendor's wording
// names another. It fails if the answer takes the unit's read, about an
// issue the vendor did not ask about, rather than read the vendor's key.
func TestAVendorsKeysAreReadOverTheUnits(t *testing.T) {
	const unit = "what about ABC-12 on the realtime voice"
	r := newRig(t)
	voiceIssue(r)
	r.exchange.AskerTurn = unit
	r.concierge.Heard(context.Background(), &r.exchange, unit)
	eventually(t, "the read of ABC-12", func() bool { return slices.Contains(r.records.calls(), "ABC-12") })
	time.Sleep(100 * time.Millisecond)
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("ABC-13 is done."))}}
	answerTurn(t, r, "What about ABC-13 on the real-time voice?")
	if !slices.Contains(r.records.calls(), "ABC-13") {
		t.Errorf("issue reads %q, want the vendor's ABC-13 read", r.records.calls())
	}
}

// TestTheWordsAnAnswerTookAreNotReadAhead (found in review on 2026-11-24):
// voices B and C tell the Concierge the unit's final words while it answers.
// It fails if words an answer already took start a read once that answer
// took its reading, a search or a key's read: no one uses it, a search
// spends the next turn's one, and a key read again could answer the next
// turn about the issue before.
func TestTheWordsAnAnswerTookAreNotReadAhead(t *testing.T) {
	t.Run("a search", func(t *testing.T) {
		const turn = "what is happening with the realtime voice"
		r := newRig(t)
		r.concierge.searchSettle = 50 * time.Millisecond
		unclearSearch(r)
		r.records.hits[0].Score = 0.012
		r.concierge.Heard(context.Background(), &r.exchange, turn)
		eventually(t, "the read-ahead searched", func() bool { return len(r.records.searches()) == 1 })
		r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("PR 80 is in review."))}}
		answerTurn(t, r, turn)
		r.concierge.Heard(context.Background(), &r.exchange, turn)
		time.Sleep(200 * time.Millisecond)
		if got := r.records.searches(); len(got) != 1 {
			t.Errorf("full searches %q, want the read-ahead's alone", got)
		}
	})
	t.Run("a key", func(t *testing.T) {
		const turn = "what about ABC-12 on the realtime voice"
		r := newRig(t)
		voiceIssue(r)
		reads := func() int {
			return len(slices.DeleteFunc(r.records.calls(), func(k string) bool { return k != "ABC-12" }))
		}
		r.concierge.Heard(context.Background(), &r.exchange, turn)
		eventually(t, "the read of ABC-12", func() bool { return reads() >= 2 })
		time.Sleep(100 * time.Millisecond)
		r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("ABC-12 is in progress."))}}
		answerTurn(t, r, turn)
		r.concierge.Heard(context.Background(), &r.exchange, turn)
		time.Sleep(200 * time.Millisecond)
		if got := reads(); got != 2 {
			t.Errorf("ABC-12 read %d times, want the read-ahead's read alone", got)
		}
	})
}

// TestAStaleReadAheadDoesNotSearchAfterTheAnswerStarts (found in review on
// 2026-11-24): an answer that searches its own question leaves no read-ahead
// of other words waiting to settle, which would send Dispatch and its
// embedding model a question no one uses and spend one of the answer's two
// searches. It fails if the read-ahead's words are searched.
func TestAStaleReadAheadDoesNotSearchAfterTheAnswerStarts(t *testing.T) {
	r := newRig(t)
	unclearSearch(r)
	r.records.hits[0].Score = 0.012
	r.concierge.searchSettle = 200 * time.Millisecond
	r.records.kwDelay = 500 * time.Millisecond
	r.concierge.Heard(context.Background(), &r.exchange, "what is happening with the realtime voice")
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("PR 80 is in review."))}}
	answerTurn(t, r, "what is happening with the realtime voice rollout")
	time.Sleep(300 * time.Millisecond)
	if got := r.records.searches(); !slices.Equal(got, []string{"what is happening with the realtime voice rollout"}) {
		t.Errorf("full searches %q, want the answer's alone", got)
	}
}

// TestASearchKeepsWithinDispatchsLimits: Dispatch refuses a search of fewer
// than 2 or more than 1000 characters with a 400, which the room would hear
// as Dispatch out of reach. It fails if a question whose only words are the
// wake name, as Deepgram spells it, and words of no work is searched, or a
// search is sent more than 1000 characters or cut inside a word.
func TestASearchKeepsWithinDispatchsLimits(t *testing.T) {
	for _, query := range []string{"Wispa?", "Lispr, are you there?", "wisper a"} {
		r := newRig(t)
		result := r.run(t, "search_issues", map[string]string{"query": query})
		r.records.mu.Lock()
		searched := len(r.records.queries) + len(r.records.kwQueries)
		r.records.mu.Unlock()
		if result["error"] == nil || searched != 0 {
			t.Errorf("%q: %v, %d searches; want none, and the model told to say what to search", query, result, searched)
		}
	}
	words := make([]string, 120)
	for i := range words {
		words[i] = fmt.Sprintf("workword%03d", i)
	}
	long := strings.Join(words, " ")
	r := newRig(t)
	r.run(t, "search_issues", map[string]string{"query": long})
	r.records.mu.Lock()
	full, kw := slices.Clone(r.records.queries), slices.Clone(r.records.kwQueries)
	r.records.mu.Unlock()
	for _, sent := range append(full, kw...) {
		if len(sent) > 1000 || !strings.HasPrefix(long, sent+" ") {
			t.Errorf("sent %d characters, %q…; want at most 1000, cut between words", len(sent), sent[:min(len(sent), 40)])
		}
	}
	if len(full) != 1 || len(kw) != 1 {
		t.Errorf("full %d, keyword %d searches; want one of each", len(full), len(kw))
	}
}

// TestAnAnswerTakesNoReadOfTheAnswerBefore: an answer takes its read-ahead's
// reading, and the count of its searches starts over; a later answer in the
// exchange reuses only a read made since. It fails if the same question asked
// again is answered from the read the answer before took.
func TestAnAnswerTakesNoReadOfTheAnswerBefore(t *testing.T) {
	r := newRig(t)
	voiceIssue(r)
	reads := func() int {
		return len(slices.DeleteFunc(r.records.calls(), func(k string) bool { return k != "ABC-12" }))
	}
	r.concierge.Heard(context.Background(), &r.exchange, "what about ABC-12")
	eventually(t, "the read of ABC-12", func() bool { return reads() == 2 })
	time.Sleep(100 * time.Millisecond)
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("ABC-12 is in progress."))}, {chunk(text("ABC-12 is in progress."))}}
	answerTurn(t, r, "what about ABC-12")
	answerTurn(t, r, "what about ABC-12")
	if got := reads(); got != 4 {
		t.Errorf("ABC-12 read %d times, want the read-ahead's 2 and the second answer's 2", got)
	}
}

// TestAPersonNamedFirstStaysInTheQuestion (Sol's review of #192 at
// 4e52421): a question may open with a person's name ("Wispr, Miss Burr
// fixed the realtime voice"), and the Supervisor hands it on without the
// wake it recognised. It fails if the full search loses the name, which main
// kept: no list of names strips the question's first words.
func TestAPersonNamedFirstStaysInTheQuestion(t *testing.T) {
	for _, tc := range []struct{ query, full string }{
		{"Miss Burr fixed the realtime voice. What changed?", "miss burr fixed the realtime voice what changed"},
		{"Miss Murray fixed the realtime voice. What changed?", "miss murray fixed the realtime voice what changed"},
		{"Mister Lopez fixed the realtime voice. What changed?", "mister lopez fixed the realtime voice what changed"},
		{"What did Miss Burr say about the realtime voice?", "what did miss burr say about the realtime voice"},
		{"What did Mister Lopez say about the realtime voice?", "what did mister lopez say about the realtime voice"},
	} {
		r := newRig(t)
		r.run(t, "search_issues", map[string]string{"query": tc.query})
		r.records.mu.Lock()
		full := slices.Clone(r.records.queries)
		r.records.mu.Unlock()
		if !slices.Equal(full, []string{tc.full}) {
			t.Errorf("search_issues(%q): full %q, want %q", tc.query, full, tc.full)
		}
	}
}
