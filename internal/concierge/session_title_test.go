package concierge

import (
	"encoding/json"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/dispatch"
)

// legionIssue is INFRA-17, claimed by the live session agent-7 whose title is
// as Legion names its sessions, "Legion architect · INFRA-17", with an event
// of INFRA-12's reviewer and one of its own architect.
func legionIssue(r *rig) {
	issue := claimed("INFRA-17", "session", "agent-7", "Legion architect · INFRA-17")
	issue.Title, issue.Status, issue.HolderState, issue.UpdatedAt = "Realtime voice", "todo", "live", testNow.Add(-time.Hour)
	r.records.issues["INFRA-17"] = issue
	r.records.events["INFRA-17"] = []dispatch.Event{
		wrote("comment.created", testNow.Add(-time.Minute), "rev-12", "Legion reviewer · INFRA-12"),
		wrote("artifact.version", testNow.Add(-time.Hour), "agent-7", "Legion architect · INFRA-17"),
	}
	r.records.events["INFRA-17"][0].Payload = json.RawMessage(`{"body":"INFRA-12 waits on this."}`)
}

// TestASessionTitleIsSaidWithoutItsSeparator (live on 2026-11-25): the room
// heard "INFRA-7 is held by Legion architect · INFRA-7": issue_holder gave the
// model the session's title as Legion writes it, a separator and the issue's
// key in it. It fails unless what the tools give the model of a session's
// title holds no separator, and a Legion session of the issue is its role
// ("its architect"), so the issue's key is said once; another issue's
// session is that issue's role ("INFRA-12's reviewer").
func TestASessionTitleIsSaidWithoutItsSeparator(t *testing.T) {
	r := newRig(t)
	legionIssue(r)
	if holder := r.run(t, "issue_holder", map[string]string{"key": "INFRA-17"}); holder["title"] != "its architect" {
		t.Errorf("issue_holder title = %q, want %q", holder["title"], "its architect")
	}
	read := r.run(t, "read_issue", map[string]string{"key": "INFRA-17"})
	events, _ := read["recent_events"].([]any)
	var by []string
	for _, event := range events {
		entry, _ := event.(map[string]any)
		name, _ := entry["by"].(string)
		by = append(by, name)
	}
	if !slices.Equal(by, []string{"INFRA-12's reviewer", "its architect"}) {
		t.Errorf("read_issue's events are by %q, want %q", by, []string{"INFRA-12's reviewer", "its architect"})
	}
	r.concierge.joined = func() []JoinedAgent {
		return []JoinedAgent{{SessionID: "agent-7", Title: "Legion implementer · INFRA-17", IssueKey: "INFRA-17"}}
	}
	joined := r.run(t, "list_joined_agents", map[string]string{})
	if agents, _ := joined["agents"].([]any); len(agents) != 1 || agents[0].(map[string]any)["title"] != "its implementer" {
		t.Errorf("list_joined_agents = %v, want the agent on INFRA-17 titled %q", joined, "its implementer")
	}
}

// TestTheHolderIsSpokenByItsRole: through Answer, the model reads INFRA-17's
// record and holder. It fails unless the record holds no separator, and the
// sentence the model writes from it, "INFRA-17 is held by its architect.",
// is said, with no separator and the key once. At c1a03f3 the record held
// "Legion architect · INFRA-17".
func TestTheHolderIsSpokenByItsRole(t *testing.T) {
	const fact = "INFRA-17 is held by its architect."
	r := newRig(t)
	legionIssue(r)
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text(fact))}}
	spoken := answerTurn(t, r, "who is working on INFRA-17")
	if len(r.model.requests) == 0 {
		t.Fatalf("the model was not asked; spoken %+v", spoken)
	}
	var record strings.Builder
	for _, content := range r.model.requests[0] {
		for _, part := range content.Parts {
			record.WriteString(part.Text)
		}
	}
	if strings.ContainsAny(record.String(), "·•|") {
		t.Errorf("the model's record holds a title's separator:\n%s", record.String())
	}
	heard := slices.ContainsFunc(spoken, func(s said) bool { return s.text == fact })
	if !heard || slices.ContainsFunc(spoken, func(s said) bool {
		return strings.ContainsAny(s.text, "·•|") || strings.Count(s.text, "INFRA-17") > 1
	}) {
		t.Errorf("spoken %+v; want %q, no separator and the key once a sentence", spoken, fact)
	}
}

// TestASessionNameIsItsWords: sessionName's table, over Sol's review of #198
// at 8821f65, each title Legion gives a session, and each shape of title
// live Dispatch's sessions had on 2026-11-25 (1,373 sessions, their words
// made up here). A title is said as its parts apart from their separators
// ("·", "•", "|", an en or em dash, a hyphen that stands alone, at either end
// too), joined by commas, an emoji gone (spokenName), and the issue's own key
// left out wherever it stands as a word. A Legion session's title, "Legion
// <role> · <KEY>", is the role of that issue, its role words as said ("its
// architect", "INFRA-12's merge queue"); "Legion <role> · <PROJECT>" is the
// project's Legion role ("INFRA's Legion controller").
func TestASessionNameIsItsWords(t *testing.T) {
	for _, tc := range []struct{ title, key, want string }{
		// Legion's phase roles (legion-roles.ts, LEGION_ROLES), of the issue
		// and of another: "Legion <role> · <issue>" (pi-legion's
		// legionSessionTitle, claudebridge's Title).
		{"Legion architect · INFRA-7", "INFRA-7", "its architect"},
		{"Legion planner · INFRA-7", "INFRA-7", "its planner"},
		{"Legion implementer · INFRA-7", "INFRA-7", "its implementer"},
		{"Legion tester · INFRA-7", "INFRA-7", "its tester"},
		{"Legion reviewer · INFRA-7", "INFRA-7", "its reviewer"},
		{"Legion merger · INFRA-7", "INFRA-7", "its merger"},
		{"Legion architect · INFRA-12", "INFRA-7", "INFRA-12's architect"},
		{"Legion planner · INFRA-12", "INFRA-7", "INFRA-12's planner"},
		{"Legion implementer · INFRA-12", "INFRA-7", "INFRA-12's implementer"},
		{"Legion tester · INFRA-12", "INFRA-7", "INFRA-12's tester"},
		{"Legion reviewer · INFRA-12", "INFRA-7", "INFRA-12's reviewer"},
		{"Legion merger · INFRA-12", "INFRA-7", "INFRA-12's merger"},
		// A role of several words, as a project's merge-queue role (Sol's O2),
		// or one with a hyphen, said as words.
		{"Legion merge queue · INFRA-17", "INFRA-17", "its merge queue"},
		{"Legion merge queue · INFRA-12", "INFRA-17", "INFRA-12's merge queue"},
		{"Legion merge-queue · INFRA-17", "INFRA-17", "its merge queue"},
		{"Legion sub-architect · INFRA-12", "INFRA-17", "INFRA-12's sub architect"},
		// The controller and the daemon name a project, not an issue.
		{"Legion controller · INFRA", "INFRA-7", "INFRA's Legion controller"},
		{"Legion daemon · INFRA", "INFRA-7", "INFRA's Legion daemon"},
		{"Legion controller", "INFRA-7", "Legion controller"},
		// Case, other separators, and no issue to speak of.
		{"Legion implementer · infra-7", "INFRA-7", "its implementer"},
		{"Legion architect • INFRA-7", "INFRA-7", "its architect"},
		{"Legion architect · INFRA-7", "", "INFRA-7's architect"},
		{"Legion architect", "INFRA-7", "Legion architect"},
		// A title that only looks like a role keeps its words.
		{"Legion keeps the repository · INFRA-7", "INFRA-7", "Legion keeps the repository"},
		{"Legion daily report", "INFRA-7", "Legion daily report"},
		// A worker's name with an issue: "<emoji> <name> · <KEY>".
		{"🦇 spiffy-bat · ABC-611", "ABC-611", "spiffy-bat"},
		{"🦇 spiffy-bat · ABC-611", "INFRA-7", "spiffy-bat, ABC-611"},
		// A task title: "<emoji> <TAG>: <sentence>", a key in it or not.
		{"🍓 PI: Deploy the unit", "INFRA-7", "PI: Deploy the unit"},
		{"📋 INFRA: Follow INFRA-5310 lead brief", "INFRA-5310", "INFRA: Follow lead brief"},
		{"📋 INFRA: Follow INFRA-5310 lead brief", "INFRA-7", "INFRA: Follow INFRA-5310 lead brief"},
		{"📨 3684: Handle INFRA-3684 Dispatch ask", "INFRA-3684", "3684: Handle Dispatch ask"},
		{"🔑 LOGIN: Build INFRA-17's box tools login", "INFRA-17", "LOGIN: Build box tools login"},
		// The issue's own key inside a part (Sol's O3), and only that key.
		{"INFRA-17 Fix pause | reviewer", "INFRA-17", "Fix pause, reviewer"},
		{"Review INFRA-17 before INFRA-170", "INFRA-17", "Review before INFRA-170"},
		{"XINFRA-17 notes", "INFRA-17", "XINFRA-17 notes"},
		// A key that ends its part takes the preposition before it along,
		// which would have no object; one in the middle leaves it.
		{"Worker on ABC-12", "ABC-12", "Worker"},
		{"Worker on ABC-12.", "ABC-12", "Worker"},
		{"Worker for infra-17", "INFRA-17", "Worker"},
		{"Notes on ABC-12 rollout", "ABC-12", "Notes on rollout"},
		{"Worker on ABC-12", "ABC-13", "Worker on ABC-12"},
		// A tmux window and a directory with a short id, and bare names.
		{"0:lib-L2 0cba5e30", "INFRA-7", "0:lib-L2 0cba5e30"},
		{"legion 3084299d", "INFRA-7", "legion 3084299d"},
		{"split:review aaaaaaaa", "INFRA-7", "split:review aaaaaaaa"},
		{"zlib-roundtrip-TL", "INFRA-7", "zlib-roundtrip-TL"},
		{"Parser-evals", "INFRA-7", "Parser-evals"},
		// Free text with a dash between its clauses, cut short.
		{"btw: how does this work - what is a hypervisor, what are the dif...", "INFRA-7", "btw: how does this work, what is a hypervisor, what are the dif..."},
		// Separators: between parts, and at either end (Sol's O1).
		{"Worker | INFRA-7", "INFRA-7", "Worker"},
		{"Worker | realtime voice", "INFRA-17", "Worker, realtime voice"},
		{"Worker — realtime voice", "INFRA-7", "Worker, realtime voice"},
		{"Worker – realtime voice", "INFRA-7", "Worker, realtime voice"},
		{"Release - INFRA-7 - notes", "INFRA-7", "Release, notes"},
		{"Worker - ", "INFRA-17", "Worker"},
		{"- Worker", "INFRA-17", "Worker"},
		{"· Worker ·", "INFRA-17", "Worker"},
		{"-", "INFRA-17", ""},
		// A run of separators and spaces is one boundary, at either end too
		// (Sol's review of #198, round 2, O1).
		{"- -", "INFRA-17", ""},
		{"- - Worker", "INFRA-17", "Worker"},
		{"Worker - -", "INFRA-17", "Worker"},
		{"Worker - - notes", "INFRA-17", "Worker, notes"},
		{"- - Worker - -", "INFRA-17", "Worker"},
		{"Worker - · notes", "INFRA-17", "Worker, notes"},
		{"Worker · - notes", "INFRA-17", "Worker, notes"},
		{"Worker ·· | notes", "INFRA-17", "Worker, notes"},
		{"Worker -- notes", "INFRA-17", "Worker, notes"},
		{"Worker—notes", "INFRA-17", "Worker, notes"},
		// Brackets the own key leaves empty go with it (Sol's round 2, I).
		{"Worker (INFRA-17)", "INFRA-17", "Worker"},
		{"Legion architect · (INFRA-17)", "INFRA-17", "its architect"},
		{"Worker (INFRA-12) | reviewer", "INFRA-17", "Worker (INFRA-12), reviewer"},
		{"INFRA-7", "INFRA-7", "its agent"},
		{"", "INFRA-7", ""},
	} {
		if got := sessionName(tc.title, tc.key); got != tc.want {
			t.Errorf("sessionName(%q, %q) = %q, want %q", tc.title, tc.key, got, tc.want)
		}
	}
}

// TestNoTitleSaysAMarkAlone (Sol's review of #198, round 2): for random
// titles built from words, issue keys and separators, in any order and
// spacing, sessionName's output holds no separator and no word made only of
// punctuation ("-", "·", "()"), which the voice would read out. The seed is
// fixed, so a failure repeats; it prints the title. It fails if a run of
// separators, or a key in brackets, leaves a mark to say.
func TestNoTitleSaysAMarkAlone(t *testing.T) {
	words := []string{"Worker", "release", "notes", "Parser-evals", "night-shift", "(reviewer)", "OWNER:", "Legion", "architect", "merge", "queue", "🔑"}
	keys := []string{"INFRA-17", "infra-17", "INFRA-17's", "(INFRA-17)", "[INFRA-17]", "INFRA-12", "INFRA-170", "XINFRA-17"}
	separators := []string{"·", "•", "‧", "∙", "|", "—", "–", "-", "--", "- -", "· -"}
	spaces := []string{"", " ", "  ", "\t"}
	rng := rand.New(rand.NewPCG(194, 198))
	for range 20000 {
		var b strings.Builder
		for n := rng.IntN(7); n >= 0; n-- {
			switch rng.IntN(3) {
			case 0:
				b.WriteString(words[rng.IntN(len(words))])
			case 1:
				b.WriteString(keys[rng.IntN(len(keys))])
			default:
				b.WriteString(separators[rng.IntN(len(separators))])
			}
			b.WriteString(spaces[rng.IntN(len(spaces))])
		}
		title := b.String()
		got := sessionName(title, "INFRA-17")
		if strings.ContainsAny(got, "·•‧∙|—–") {
			t.Fatalf("sessionName(%q) = %q, which holds a separator", title, got)
		}
		for _, word := range strings.FieldsFunc(got, func(r rune) bool { return r == ',' || unicode.IsSpace(r) }) {
			if !strings.ContainsFunc(word, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) {
				t.Fatalf("sessionName(%q) = %q: %q is punctuation alone", title, got, word)
			}
		}
	}
}

// TestTheHolderToolSaysTheTitlesWords (Sol's review of #198, O2 and O3):
// through issue_holder, a multiword Legion role is the issue's, and the
// issue's own key inside a title is left out.
func TestTheHolderToolSaysTheTitlesWords(t *testing.T) {
	for _, tc := range []struct{ key, title, want string }{
		{"INFRA-17", "Legion merge queue · INFRA-17", "its merge queue"},
		{"INFRA-17", "Legion merge queue · INFRA-12", "INFRA-12's merge queue"},
		{"INFRA-17", "INFRA-17 Fix pause | reviewer", "Fix pause, reviewer"},
	} {
		r := newRig(t)
		issue := claimed(tc.key, "session", "agent-7", tc.title)
		issue.HolderState = "live"
		r.records.issues[tc.key] = issue
		if holder := r.run(t, "issue_holder", map[string]string{"key": tc.key}); holder["title"] != tc.want {
			t.Errorf("issue_holder(%s), session title %q: title %q, want %q", tc.key, tc.title, holder["title"], tc.want)
		}
	}
}
