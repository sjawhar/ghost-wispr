package concierge

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/envoy"
)

// TestAQuestionToAnAgentIsAtMost300Characters: R7 bounds the words a
// question carries out of the unit to 300 characters. A question of 300 goes;
// one of 301 sends nothing and records no task.
func TestAQuestionToAnAgentIsAtMost300Characters(t *testing.T) {
	r := newRig(t)
	r.name(t, "agent-7")
	long := strings.Repeat("é", 301)
	if result := r.run(t, "ask_agent", map[string]string{"session": "agent-7", "question": long}); result["error"] == nil {
		t.Fatalf("a question of 301 characters was sent: %v", result)
	}
	if len(r.agents.sent) != 0 {
		t.Fatalf("sent %d messages for a question of 301 characters", len(r.agents.sent))
	}
	if tasks, err := r.store.VoiceTasks("meeting-1"); err != nil || len(tasks) != 0 {
		t.Fatalf("tasks = %+v, %v; want none", tasks, err)
	}
	if result := r.run(t, "ask_agent", map[string]string{"session": "agent-7", "question": long[:len(long)-len("é")]}); result["error"] != nil {
		t.Fatalf("a question of 300 characters was refused: %v", result)
	}
	if len(r.agents.sent) != 1 {
		t.Fatalf("sent %d messages for a question of 300 characters, want 1", len(r.agents.sent))
	}
}

// TestEarCheck (c-ear): R8's check on made-up pairs. It refuses a changed
// key, a changed number, a dropped key of the question, a name the source
// does not hold, a path, a backtick and a bar, and accepts a sentence that
// starts with a capitalised word the source does not hold and a long
// identifier cut to its first 7 characters.
func TestEarCheck(t *testing.T) {
	const written = "Yes: `PR #80` merged the tap at 08:10 UTC for Sami (commit a3f9c21d4e5f6a7b, see `internal/voice/tap.go`). 4.5% of tests ran. ABC-12 is next. Trace 0123456789abcdef0123456789abcdef01234567 holds it."
	for _, c := range []struct {
		name, text, key string
		fails           bool
	}{
		{"keeps the written facts", "ABC-12: PR 80 merged the tap at 08:10 UTC for Sami.", "ABC-12", false},
		{"a cut commit", "Commit a3f9c21 merged the tap.", "", false},
		{"a cut 40-character identifier", "Trace 0123456 holds it.", "", false},
		{"a capitalised first word the source does not hold", "Indeed, the tap merged.", "", false},
		{"a percentage", "Only 4.5% of tests ran.", "", false},
		{"a percentage said in words", "Only 4.5 percent of tests ran.", "", false},
		{"a changed percentage", "Only 4.6 percent of tests ran.", "", true},
		{"the question's key", "ABC-12: the tap merged.", "ABC-12", false},
		{"a changed key", "ABC-13 is next.", "", true},
		{"an invented key", "INFRA-4592 is resolved.", "", true},
		{"a dropped key of the question", "The tap merged.", "ABC-12", true},
		{"a changed pull request", "PR 81 merged the tap.", "", true},
		{"a pull request named as a number", "Pull request 80 merged the tap.", "", false},
		{"a changed figure", "The tap merged at 9:10.", "", true},
		{"a figure written otherwise", "The tap merged at 8:10.", "", true},
		{"a name the source does not hold", "The tap merged, and Alex approved it.", "", true},
		{"a name from the source", "The tap merged for Sami.", "", false},
		{"a commit cut too short", "Commit a3f9c2 merged the tap.", "", true},
		{"a path", "See internal/voice/tap.go for it.", "", true},
		{"a slash in a word", "The tap and/or the mixer merged.", "", true},
		{"a backtick", "The `tap` merged.", "", true},
		{"a bar", "The tap | merged.", "", true},
		{"four sentences", "It merged. It ran. It passed. It shipped.", "", true},
		{"a long sentence", "The " + strings.Repeat("tap ", 20) + "merged.", "", true},
		{"more than 50 words", strings.Repeat("one two three four five six seven eight nine ten eleven twelve thirteen fourteen fifteen sixteen seventeen. ", 3), "", true},
	} {
		reason := earCheck(c.text, written, c.key)
		if (reason != "") != c.fails {
			t.Errorf("%s: earCheck(%q) = %q, want failing %v", c.name, c.text, reason, c.fails)
		}
		if reason != "" && strings.Contains(reason, "Alex") {
			t.Errorf("%s: the reason quotes the text: %q", c.name, reason)
		}
	}
	// The check's negative control: PR 97 for a source that says PR 79.
	if earCheck("PR 97 merged.", "PR 79 merged.", "") == "" {
		t.Error("PR 97 passed for a source that says PR 79")
	}
}

// TestARewriteThatInventsIsWrittenAgain: the first rewrite names an issue the
// written answer does not; the second, which passes, is spoken. The model is
// told why, and the log says why, with no words of either text.
func TestARewriteThatInventsIsWrittenAgain(t *testing.T) {
	r := newRig(t)
	logs := r.captureLogs()
	r.model.turns = [][]*genai.GenerateContentResponse{
		{chunk(text("INFRA-4592 is resolved, and the answer is pong."))},
		{chunk(text("Pong."))},
	}
	spoken := r.concierge.forEar(context.Background(), "pong", "")
	if spoken != "Pong." {
		t.Fatalf("spoken = %q, want the second rewrite", spoken)
	}
	if len(r.model.requests) != 2 {
		t.Fatalf("model calls = %d, want 2", len(r.model.requests))
	}
	second := r.model.requests[1][0].Parts[0].Text
	if !strings.Contains(second, "issue key") || strings.Contains(second, "INFRA-4592") {
		t.Errorf("the second request = %q, want the reason and not the failed text", second)
	}
	if !strings.Contains(logs.String(), "issue key") || strings.Contains(logs.String(), "INFRA-4592") || strings.Contains(logs.String(), "pong") {
		t.Errorf("the log = %q, want the reason and no words", logs)
	}
}

// TestTwoFailedRewritesSpeakTheFixedSentence: after a second failure the
// voice says the fixed sentence; the log says it fell back, and why.
func TestTwoFailedRewritesSpeakTheFixedSentence(t *testing.T) {
	r := newRig(t)
	logs := r.captureLogs()
	r.model.turns = [][]*genai.GenerateContentResponse{
		{chunk(text("Alex merged PR 81."))},
		{chunk(text("Alex merged PR 81 yesterday."))},
	}
	if spoken := r.concierge.forEar(context.Background(), "PR #80 merged.", ""); spoken != fixedSentence {
		t.Fatalf("spoken = %q, want the fixed sentence", spoken)
	}
	r.model.turns = append(r.model.turns, []*genai.GenerateContentResponse{chunk(text("PR 81 merged it."))}, []*genai.GenerateContentResponse{chunk(text("PR 81 merged it."))})
	if spoken := r.concierge.forEar(context.Background(), "PR #80 merged.", "ABC-12"); spoken != "ABC-12: "+fixedSentence {
		t.Fatalf("spoken = %q, want the fixed sentence with the question's key (R8)", spoken)
	}
	if !strings.Contains(logs.String(), "fixed sentence") || strings.Contains(logs.String(), "Alex") || strings.Contains(logs.String(), "merged") {
		t.Errorf("the log = %q, want the fall-back and its reason, no words", logs)
	}
}

// TestTheQuestionsKeyIsSpoken: R8 asks that the issue key the question named
// be in the text; a rewrite without it gets it in front.
func TestTheQuestionsKeyIsSpoken(t *testing.T) {
	r := newRig(t)
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("Pong."))}}
	if spoken := r.concierge.forEar(context.Background(), "pong", "ABC-12"); spoken != "ABC-12: Pong." {
		t.Fatalf("spoken = %q", spoken)
	}
	r.model.turns = append(r.model.turns, []*genai.GenerateContentResponse{chunk(text("On ABC-12, pong."))})
	if spoken := r.concierge.forEar(context.Background(), "pong", "ABC-12"); spoken != "On ABC-12, pong." {
		t.Fatalf("spoken = %q, want the rewrite as it is when it names the key", spoken)
	}
}

// TestAnEndedMeetingsAnswerGoesToNoModel: an answer that lands after its
// meeting ended is stored and not sent to the model for the ear: it will not
// be spoken.
func TestAnEndedMeetingsAnswerGoesToNoModel(t *testing.T) {
	for _, viaHook := range []bool{true, false} {
		r := newRig(t)
		r.ask(t, "agent-7", "q", "")
		if viaHook {
			if err := r.concierge.SessionEnded("meeting-1"); err != nil {
				t.Fatal(err)
			}
		} else if err := r.store.EndSession("meeting-1", testNow, ""); err != nil {
			t.Fatal(err)
		}
		r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("Done."))}}
		if err := r.concierge.Deliver(context.Background(), &envoy.Message{EventID: "r", SourceSession: "agent-7", InReplyTo: "evt-1", Text: "Done."}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(300 * time.Millisecond)
		r.model.mu.Lock()
		calls := len(r.model.requests)
		r.model.mu.Unlock()
		if calls != 0 {
			t.Fatalf("ended through the hook %v: the answer went to the model %d times", viaHook, calls)
		}
		if task, _ := r.store.VoiceTask("evt-1"); task.AnswerWritten != "Done." {
			t.Fatalf("the answer was not kept: %+v", task)
		}
	}
}

// TestAKeyIsNotReadBeforeTheProjectsAreKnown: until Dispatch's projects are
// known, or when their read fails, no "word NN" is read as an issue key, so
// "the last 30 days" never reaches Dispatch as a path.
func TestAKeyIsNotReadBeforeTheProjectsAreKnown(t *testing.T) {
	r := newRig(t)
	voiceIssue(r)
	r.records.mu.Lock()
	r.records.projects = nil
	r.records.mu.Unlock()
	r.concierge.mu.Lock()
	r.concierge.projects, r.concierge.projectsRead = nil, time.Time{}
	r.concierge.mu.Unlock()
	const turn = "what changed on the realtime voice in the last 30 days"
	r.concierge.Heard(context.Background(), &r.exchange, turn)
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("ABC-12 is in progress."))}}
	var spoken []said
	if err := r.concierge.Answer(context.Background(), &r.exchange, turn, recorder(&spoken)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if calls := r.records.calls(); slices.Contains(calls, "LAST-30") {
		t.Fatalf("issue reads = %q, want no LAST-30", calls)
	}
}

// TestRewriteForEarReportsItsTries: the exported rewrite, for the ear-corpus
// tool, is the holder's-answer path, and says whether the check passed and
// on which try.
func TestRewriteForEarReportsItsTries(t *testing.T) {
	r := newRig(t)
	r.model.turns = [][]*genai.GenerateContentResponse{
		{chunk(text("Pong."))},
		{chunk(text("PR 81 merged."))}, {chunk(text("Pong, PR 80."))},
		{chunk(text("PR 81 merged."))}, {chunk(text("PR 82 merged."))},
	}
	for _, want := range []EarRewrite{
		{Text: "Pong.", Tries: 1, Passed: true},
		{Text: "Pong, PR 80.", Tries: 2, Passed: true},
		{Text: fixedSentence, Tries: 2, Passed: false},
	} {
		got, err := r.concierge.RewriteForEar(context.Background(), "Pong. PR #80 merged.", "")
		if err != nil || got != want {
			t.Fatalf("RewriteForEar = %+v, %v; want %+v", got, err, want)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.concierge.RewriteForEar(ctx, "x", ""); err == nil {
		t.Fatal("a stopped rewrite reported no error")
	}
}
