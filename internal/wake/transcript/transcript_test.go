package transcript

import (
	"slices"
	"strings"
	"testing"
)

func TestWakePhrasePosition(t *testing.T) {
	d := New([]string{"Wispr", "Ghost Wispr"})
	cases := []struct {
		text   string
		wake   bool
		phrase string
		rest   string
	}{
		{"Wispr, what is happening with the realtime voice issue?", true, "Wispr", "what is happening with the realtime voice issue?"},
		{"Ghost Wispr, what is happening?", true, "Ghost Wispr", "what is happening?"},
		{"Hey Wispr, who holds it?", true, "Wispr", "who holds it?"},
		{"Okay. Wispr, go ahead.", true, "Wispr", "go ahead."},
		{"wispr what is happening", true, "Wispr", "what is happening"},
		// Deepgram may spell the name as the word; at the start of a
		// sentence and followed by a comma or a question word it is a call.
		{"Whisper, what is happening?", true, "Wispr", "what is happening?"},
		{"Whisper what is the status?", true, "Wispr", "what is the status?"},
		{"Wispr", true, "Wispr", ""},
		// Not calls.
		{"I will whisper it to you", false, "", ""},
		{"Whisper it to me later.", false, "", ""},
		{"We asked Wispr yesterday.", false, "", ""},
		{"The ghost whisper film was good.", false, "", ""},
		{"", false, "", ""},
	}
	for _, c := range cases {
		m, ok := d.Find(c.text)
		if ok != c.wake {
			t.Errorf("Find(%q) woke = %v, want %v", c.text, ok, c.wake)
			continue
		}
		if !ok {
			continue
		}
		if m.Phrase != c.phrase || m.Rest != c.rest {
			t.Errorf("Find(%q) = {%q, %q}, want {%q, %q}", c.text, m.Phrase, m.Rest, c.phrase, c.rest)
		}
	}
}

func TestCommands(t *testing.T) {
	cases := map[string]Command{
		"stop":                         Stop,
		"stop.":                        Stop,
		"Stop talking, please.":        Stop,
		"be quiet":                     Stop,
		"that's enough":                Stop,
		"go ahead":                     GoAhead,
		"Go ahead.":                    GoAhead,
		"go on":                        GoAhead,
		"go ahead, INFRA 1027":         GoAhead,
		"never mind":                   NeverMind,
		"Never mind.":                  NeverMind,
		"nevermind":                    NeverMind,
		"forget it":                    NeverMind,
		"who has a hand up?":           HandsUp,
		"Who has their hand up?":       HandsUp,
		"who's got a hand up":          HandsUp,
		"any hands up?":                HandsUp,
		"who handles the budget?":      Ask,
		"who is on INFRA 1027?":        Ask,
		"what is happening with this?": Ask,
		"":                             Ask,
	}
	for rest, want := range cases {
		if got := CommandOf(rest); got != want {
			t.Errorf("CommandOf(%q) = %v, want %v", rest, got, want)
		}
	}
}

// TestKeyterms fails if the keyterms do not start with the wake phrases, once
// each, or pass Deepgram's limits.
func TestKeyterms(t *testing.T) {
	keys := make([]string, 0, 400)
	for i := range 400 {
		keys = append(keys, "KEY-"+strings.Repeat("1", 1+i%3)+string(rune('A'+i%26)))
	}
	got := Keyterms([]string{"Wispr", "Ghost Wispr", "Wispr"}, keys)
	if len(got) > MaxKeyterms {
		t.Fatalf("%d keyterms, want at most %d", len(got), MaxKeyterms)
	}
	if !slices.Equal(got[:2], []string{"Wispr", "Ghost Wispr"}) {
		t.Fatalf("keyterms start %q, want the wake phrases first, once each", got[:2])
	}
	tokens := 0
	for _, k := range got {
		tokens += len(strings.Fields(k))
	}
	if tokens > MaxKeytermTokens {
		t.Fatalf("%d keyterm words, want at most %d", tokens, MaxKeytermTokens)
	}
	seen := map[string]bool{}
	for _, k := range got {
		if seen[k] {
			t.Fatalf("keyterm %q twice", k)
		}
		seen[k] = true
	}
}

// fakeStream is a live Deepgram socket as LiveKeyterms reaches it: it records
// each Configure message's keyterms.
type fakeStream struct{ configured [][]string }

func (f *fakeStream) Configure(keyterms []string) error {
	f.configured = append(f.configured, slices.Clone(keyterms))
	return nil
}

// TestKeytermsChangeOnJoin fails if a join or a leave does not change the
// live stream's keyterms with one Configure message, if the joined agents'
// keys do not come right after the wake phrases, if an unchanged list is sent
// again, or if a socket dialled before a join misses it.
func TestKeytermsChangeOnJoin(t *testing.T) {
	k := NewLiveKeyterms([]string{"Wispr", "Ghost Wispr"}, func() []string { return []string{"INFRA", "OPS"} })
	base := []string{"Wispr", "Ghost Wispr", "INFRA", "OPS"}
	stream := &fakeStream{}
	k.Attach(stream, k.List())
	if !slices.Equal(k.List(), base) || len(stream.configured) != 0 {
		t.Fatalf("before a join: list %q, %d Configure messages; want %q and none", k.List(), len(stream.configured), base)
	}

	k.SetJoined([]string{"ABC-12"})
	joined := []string{"Wispr", "Ghost Wispr", "ABC-12", "INFRA", "OPS"}
	if len(stream.configured) != 1 || !slices.Equal(stream.configured[0], joined) {
		t.Fatalf("a join sent %q, want one Configure with %q", stream.configured, joined)
	}
	if !slices.Equal(k.List(), joined) {
		t.Fatalf("after a join a dial reads %q, want %q", k.List(), joined)
	}
	k.SetJoined([]string{"ABC-12"})
	if len(stream.configured) != 1 {
		t.Fatalf("an unchanged list was sent again: %q", stream.configured)
	}

	k.SetJoined(nil)
	if len(stream.configured) != 2 || !slices.Equal(stream.configured[1], base) {
		t.Fatalf("a leave sent %q, want a second Configure with %q", stream.configured, base)
	}

	// A socket dialled with the list from before a join, and attached after
	// it, is sent the list at once; the join itself went to the socket that
	// still carried the meeting.
	dialed := k.List()
	k.SetJoined([]string{"XYZ-9"})
	late := &fakeStream{}
	k.Attach(late, dialed)
	want := []string{"Wispr", "Ghost Wispr", "XYZ-9", "INFRA", "OPS"}
	if len(late.configured) != 1 || !slices.Equal(late.configured[0], want) {
		t.Fatalf("a socket dialled before the join got %q, want one Configure with %q", late.configured, want)
	}
	// From the attach on, a leave reaches the new socket only.
	before := len(stream.configured)
	k.SetJoined(nil)
	if len(late.configured) != 2 || !slices.Equal(late.configured[1], base) {
		t.Fatalf("a leave after the attach sent the new socket %q, want a second Configure with %q", late.configured, base)
	}
	if len(stream.configured) != before {
		t.Fatalf("the socket the new one replaced was sent %q", stream.configured[before:])
	}
}

// TestIsName fails if a word Deepgram writes for the name, as configured or as
// a sound-alike, is not recognised as the name, or another word is.
func TestIsName(t *testing.T) {
	d := New([]string{"Wispr", "Ghost Wispr"})
	for word, want := range map[string]bool{
		"Wispr,": true, "wispr": true, "Whisper.": true, "Ghost": true,
		"Tell": false, "whiskey": false, "": false,
	} {
		if got := d.IsName(word); got != want {
			t.Errorf("IsName(%q) = %v, want %v", word, got, want)
		}
	}
}

// TestIssueKeyForms fails if a key spoken in a common form does not read as
// its written key, or a phrase with no key reads as one.
func TestIssueKeyForms(t *testing.T) {
	cases := map[string][]string{
		"what is happening with INFRA-1027?":     {"INFRA-1027"},
		"go ahead, infra 1027":                   {"INFRA-1027"},
		"INFRA 1027 and ABC-12":                  {"INFRA-1027", "ABC-12"},
		"infra ten twenty-seven":                 {"INFRA-1027"},
		"infra ten twenty seven please":          {"INFRA-1027"},
		"infra one zero two seven":               {"INFRA-1027"},
		"infra one oh two seven":                 {"INFRA-1027"},
		"Infra-1027.":                            {"INFRA-1027"},
		"infra 1027's spec":                      {"INFRA-1027"},
		"infra fifteen":                          {"INFRA-15"},
		"infra forty":                            {"INFRA-40"},
		"infra eleven hundred":                   {"INFRA-1100"},
		"infra eleven hundred five":              {"INFRA-1105"},
		"what is happening with the voice work?": nil,
		"go ahead":                               nil,
		"I will be there in a minute":            nil,
		// As Deepgram wrote Aura's "infra 4592" in end-to-end run 8c, and as
		// people say it.
		"ask the agent on in for forty five hundred and two": {"INFRA-4502"},
		"infra forty-five ninety-two":                        {"INFRA-4592"},
		"infra four five nine two":                           {"INFRA-4592"},
		"in for forty five ninety two please":                {"INFRA-4592"},
		"I am in for a minute":                               nil,
		// End-to-end run 8e: smart formatting writes "4,592".
		"ask the agent on in For 4,592 when": {"INFRA-4592"},
	}
	for text, want := range cases {
		if got := IssueKeys(text); !slices.Equal(got, want) {
			t.Errorf("IssueKeys(%q) = %q, want %q", text, got, want)
		}
	}
}

// TestSpokenKeysAreWritten fails if a key of a known project, said in a
// common form, is not written as the key in the turn the Concierge gets, or
// if words that name no known project's issue change.
func TestSpokenKeysAreWritten(t *testing.T) {
	projects := []string{"INFRA", "ABC"}
	for text, want := range map[string]string{
		"please ask the agent on in for forty five ninety two when it ships?": "please ask the agent on INFRA-4592 when it ships?",
		"what about infra ten twenty-seven today":                             "what about INFRA-1027 today",
		"is abc 12 done":                     "is ABC-12 done",
		"what changed in the last 30 days":   "what changed in the last 30 days",
		"I am in for a minute":               "I am in for a minute",
		"what is happening with INFRA-1027?": "what is happening with INFRA-1027?",
		// "in for" reads as INFRA only before a number of three or more
		// digits, as issue numbers are; a duration is not one (ReviewPR89).
		"we're in for thirty minutes?":  "we're in for thirty minutes?",
		"we are in for 45 more minutes": "we are in for 45 more minutes",
		// Nor is a year (ReviewPR89's round 3); the project said as a word
		// still reads.
		"we're in for 2027":                "we're in for 2027",
		"we're in for twenty twenty seven": "we're in for twenty twenty seven",
		"what about infra 2027":            "what about INFRA-2027",
	} {
		if got := WriteKeys(text, projects); got != want {
			t.Errorf("WriteKeys(%q) = %q, want %q", text, got, want)
		}
	}
}

// TestThanksEndsAnExchange fails if a thank-you to Wispr is not read as one,
// or other words are.
func TestThanksEndsAnExchange(t *testing.T) {
	d := New([]string{"Wispr", "Ghost Wispr"})
	for text, want := range map[string]bool{
		"Thanks, Wispr.":         true,
		"thank you Wispr":        true,
		"Thanks Ghost Wispr!":    true,
		"thanks, whisper":        true,
		"thanks":                 false,
		"Wispr, thanks for that": false,
		"thanks, now the budget": false,
	} {
		if got := d.Thanks(text); got != want {
			t.Errorf("Thanks(%q) = %v, want %v", text, got, want)
		}
	}
}
