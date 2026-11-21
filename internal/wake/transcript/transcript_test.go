package transcript

import (
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
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
		// The office run of 2026-11-21 heard the name as "Lispr".
		{"Lispr, what's the status of INFRA ten twenty seven?", true, "Wispr", "what's the status of INFRA ten twenty seven?"},
		// Not calls.
		{"I will whisper it to you", false, "", ""},
		{"Whisper it to me later.", false, "", ""},
		{"We asked Wispr yesterday.", false, "", ""},
		{"The ghost whisper film was good.", false, "", ""},
		// "Mister" and the like are a person's address, not the name.
		{"Mister, what's the status of info 10/27?", false, "", ""},
		{"Miss Murray, what's the status?", false, "", ""},
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
// each, with "Whisper" right after "Wispr", or pass Deepgram's limits. Nova-3
// knows how "Whisper" sounds, not "Wispr", which it heard as "Mister", "Miss
// Burr" and "Lispr" on 2026-11-21.
func TestKeyterms(t *testing.T) {
	keys := make([]string, 0, 400)
	for i := range 400 {
		keys = append(keys, "KEY-"+strings.Repeat("1", 1+i%3)+string(rune('A'+i%26)))
	}
	got := Keyterms([]string{"Wispr", "Ghost Wispr", "Wispr"}, keys)
	if len(got) > MaxKeyterms {
		t.Fatalf("%d keyterms, want at most %d", len(got), MaxKeyterms)
	}
	if !slices.Equal(got[:3], []string{"Wispr", "Whisper", "Ghost Wispr"}) {
		t.Fatalf("keyterms start %q, want the wake phrases first, once each, with Whisper after Wispr", got[:3])
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
// each Configure message's keyterms, and gives the hook OnConfigured set
// Deepgram's answer when the test says (answer).
type fakeStream struct {
	mu         sync.Mutex
	configured [][]string
	answered   func(keyterms []string, taken bool)
}

func (f *fakeStream) Configure(keyterms []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.configured = append(f.configured, slices.Clone(keyterms))
	return nil
}

func (f *fakeStream) OnConfigured(answer func(keyterms []string, taken bool)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answered = answer
}

// sent returns the keyterms of each Configure so far.
func (f *fakeStream) sent() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.configured)
}

// answer gives Deepgram's answer to the newest Configure: taken, or refused.
func (f *fakeStream) answer(taken bool) {
	f.mu.Lock()
	answered, keyterms := f.answered, f.configured[len(f.configured)-1]
	f.mu.Unlock()
	answered(keyterms, taken)
}

// waitSent waits for the stream's nth Configure and returns them all.
func waitSent(t *testing.T, f *fakeStream, n int) [][]string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for len(f.sent()) < n {
		if time.Now().After(deadline) {
			t.Fatalf("the stream got %q, want %d Configure messages", f.sent(), n)
		}
		time.Sleep(time.Millisecond)
	}
	return f.sent()
}

// quickKeyterms are live keyterms whose resend of a refused list waits only
// 20 ms, attached to a stream dialled with them.
func quickKeyterms() (*LiveKeyterms, *fakeStream) {
	k := NewLiveKeyterms([]string{"Wispr"}, func() []string { return []string{"INFRA"} })
	k.retryAfter = 20 * time.Millisecond
	stream := &fakeStream{}
	k.Attach(stream, k.List())
	return k, stream
}

// TestKeytermsChangeOnJoin fails if a join or a leave does not change the
// live stream's keyterms with one Configure message, if the joined agents'
// keys do not come right after the wake phrases, if an unchanged list is sent
// again, if a socket dialled before a join misses it, or if the answer of the
// socket a new one replaced lets a second Configure go to the new one before
// it answered.
func TestKeytermsChangeOnJoin(t *testing.T) {
	k := NewLiveKeyterms([]string{"Wispr", "Ghost Wispr"}, func() []string { return []string{"INFRA", "OPS"} })
	base := []string{"Wispr", "Whisper", "Ghost Wispr", "INFRA", "OPS"}
	stream := &fakeStream{}
	k.Attach(stream, k.List())
	if !slices.Equal(k.List(), base) || len(stream.configured) != 0 {
		t.Fatalf("before a join: list %q, %d Configure messages; want %q and none", k.List(), len(stream.configured), base)
	}

	k.SetJoined([]string{"ABC-12"})
	joined := []string{"Wispr", "Whisper", "Ghost Wispr", "ABC-12", "INFRA", "OPS"}
	if len(stream.configured) != 1 || !slices.Equal(stream.configured[0], joined) {
		t.Fatalf("a join sent %q, want one Configure with %q", stream.configured, joined)
	}
	stream.answer(true)
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
	stream.answer(true)

	// A socket dialled with the list from before a join, and attached after
	// it, is sent the list at once; the join itself went to the socket that
	// still carried the meeting.
	dialed := k.List()
	k.SetJoined([]string{"XYZ-9"})
	late := &fakeStream{}
	k.Attach(late, dialed)
	want := []string{"Wispr", "Whisper", "Ghost Wispr", "XYZ-9", "INFRA", "OPS"}
	if len(late.configured) != 1 || !slices.Equal(late.configured[0], want) {
		t.Fatalf("a socket dialled before the join got %q, want one Configure with %q", late.configured, want)
	}
	// From the attach on, a leave reaches the new socket only. The replaced
	// socket's answer comes late: the new one still waits for its own before
	// it is sent the leave.
	stream.answer(true)
	before := len(stream.configured)
	k.SetJoined(nil)
	if len(late.configured) != 1 {
		t.Fatalf("a leave went before the new socket answered its first Configure: %q", late.configured)
	}
	late.answer(true)
	if len(late.configured) != 2 || !slices.Equal(late.configured[1], base) {
		t.Fatalf("a leave after the attach sent the new socket %q, want a second Configure with %q", late.configured, base)
	}
	if len(stream.configured) != before {
		t.Fatalf("the socket the new one replaced was sent %q", stream.configured[before:])
	}
}

// TestOneConfigureWaitsForItsAnswer fails if a list goes while the last
// waits for Deepgram's answer (Deepgram refuses it, CONFIGURE_ALREADY_PENDING),
// if the answer does not send the newest list, or if a refusal sends the
// refused list again although a newer one is wanted, which goes at once.
func TestOneConfigureWaitsForItsAnswer(t *testing.T) {
	k, stream := quickKeyterms()
	k.SetJoined([]string{"ABC-12"})
	k.SetJoined([]string{"ABC-12", "XYZ-9"})
	k.SetJoined([]string{"XYZ-9"})
	if got := stream.sent(); len(got) != 1 {
		t.Fatalf("the stream got %q before it answered the first Configure, want one", got)
	}
	stream.answer(true)
	if got, want := stream.sent(), []string{"Wispr", "Whisper", "XYZ-9", "INFRA"}; len(got) != 2 || !slices.Equal(got[1], want) {
		t.Fatalf("after the answer the stream got %q, want the newest list %q second", got, want)
	}
	k.SetJoined([]string{"XYZ-9", "DEF-3"})
	stream.answer(false)
	want := []string{"Wispr", "Whisper", "XYZ-9", "DEF-3", "INFRA"}
	if got := stream.sent(); len(got) != 3 || !slices.Equal(got[2], want) {
		t.Fatalf("after a refusal with a newer list wanted the stream got %q, want %q third, at once", got, want)
	}
	time.Sleep(5 * k.retryAfter)
	if got := stream.sent(); len(got) != 3 {
		t.Fatalf("the stream got %q, want the refused list not sent again once a newer one went", got)
	}
}

// TestARefusedListIsNotTheStreams fails if a list Deepgram refused, with no
// join or leave since, counts as the stream's list: it must go once more
// after retryAfter, and only Deepgram taking it makes it the stream's, after
// which the same join sends nothing. The refusal hook must say the refusal,
// and that Deepgram took the list.
func TestARefusedListIsNotTheStreams(t *testing.T) {
	k, stream := quickKeyterms()
	refused := false
	k.SetRefusalHook(func(r bool) { refused = r })
	joined := []string{"Wispr", "Whisper", "ABC-12", "INFRA"}
	k.SetJoined([]string{"ABC-12"})
	stream.answer(false)
	if !refused {
		t.Fatal("the refusal hook did not say Deepgram refused the list")
	}
	if got := waitSent(t, stream, 2); !slices.Equal(got[1], joined) {
		t.Fatalf("after a refusal the stream got %q, want %q again", got, joined)
	}
	stream.answer(true)
	if refused {
		t.Fatal("the refusal hook still says a refusal after Deepgram took the list")
	}
	k.SetJoined([]string{"ABC-12"})
	time.Sleep(5 * k.retryAfter)
	if got := stream.sent(); len(got) != 2 {
		t.Fatalf("the stream got %q after Deepgram took the list, want no third Configure", got)
	}
}

// TestASecondRefusalWaitsForTheNextChange fails if a list Deepgram refused
// twice goes again before a join or a leave, or if the next one does not send
// it again, with a resend of its own.
func TestASecondRefusalWaitsForTheNextChange(t *testing.T) {
	k, stream := quickKeyterms()
	joined := []string{"Wispr", "Whisper", "ABC-12", "INFRA"}
	k.SetJoined([]string{"ABC-12"})
	stream.answer(false)
	waitSent(t, stream, 2)
	stream.answer(false)
	time.Sleep(5 * k.retryAfter)
	if got := stream.sent(); len(got) != 2 {
		t.Fatalf("after a second refusal the stream got %q, want no third Configure before the list changes", got)
	}
	k.SetJoined([]string{"ABC-12"})
	if got := stream.sent(); len(got) != 3 || !slices.Equal(got[2], joined) {
		t.Fatalf("the next join sent %q, want %q third, at once", got, joined)
	}
	stream.answer(false)
	if got := waitSent(t, stream, 4); !slices.Equal(got[3], joined) {
		t.Fatalf("after the next join's refusal the stream got %q, want %q again", got, joined)
	}
}

// refusalCalls records what each call of the refusal hook says.
type refusalCalls struct {
	mu    sync.Mutex
	calls []bool
}

func (r *refusalCalls) hook(refused bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, refused)
}

// heard returns what the hook heard so far, in order.
func (r *refusalCalls) heard() []bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls)
}

// warns is whether the last call said a refusal.
func (r *refusalCalls) warns() bool {
	heard := r.heard()
	return len(heard) > 0 && heard[len(heard)-1]
}

// TestKeytermsRefusalEndsWhenTheStreamHasTheWantedList fails if the refusal
// hook still says a refusal once a leave makes the wanted list the one the
// stream has, with no Configure to answer: a leave after the refusal, a leave
// while the refused list's resend waits for its answer, and a leave while the
// first Configure waits for an answer that then refuses it, which must not
// warn at all. It also fails if such a leave sends a Configure.
func TestKeytermsRefusalEndsWhenTheStreamHasTheWantedList(t *testing.T) {
	t.Run("leave after the refusal", func(t *testing.T) {
		k, stream := quickKeyterms()
		k.retryAfter = time.Hour
		t.Cleanup(func() { k.Attach(nil, nil) })
		var hook refusalCalls
		k.SetRefusalHook(hook.hook)
		k.SetJoined([]string{"ABC-12"})
		stream.answer(false)
		if got := hook.heard(); !slices.Equal(got, []bool{true}) {
			t.Fatalf("control: after a refusal the hook heard %v, want [true]", got)
		}
		k.SetJoined(nil)
		if got := hook.heard(); !slices.Equal(got, []bool{true, false}) {
			t.Fatalf("the leave made the wanted list the stream's, but the hook heard %v, want [true false]", got)
		}
		if got := stream.sent(); len(got) != 1 {
			t.Fatalf("a leave to the list the stream has sent %q, want no second Configure", got)
		}
	})
	t.Run("leave while the resend waits", func(t *testing.T) {
		k, stream := quickKeyterms()
		var hook refusalCalls
		k.SetRefusalHook(hook.hook)
		k.SetJoined([]string{"ABC-12"})
		stream.answer(false)
		waitSent(t, stream, 2)
		k.SetJoined(nil)
		if got := hook.heard(); !slices.Equal(got, []bool{true, false}) {
			t.Fatalf("the leave made the wanted list the stream's while the resend waits, but the hook heard %v, want [true false]", got)
		}
		stream.answer(false)
		time.Sleep(5 * k.retryAfter)
		if got := hook.heard(); !slices.Equal(got, []bool{true, false}) {
			t.Fatalf("the resend's refusal came after the leave, but the hook heard %v, want [true false]", got)
		}
		if got := stream.sent(); len(got) != 2 {
			t.Fatalf("the stream got %q, want no third Configure for the list it has", got)
		}
	})
	t.Run("leave while the first Configure waits", func(t *testing.T) {
		k, stream := quickKeyterms()
		var hook refusalCalls
		k.SetRefusalHook(hook.hook)
		k.SetJoined([]string{"ABC-12"})
		k.SetJoined(nil)
		stream.answer(false)
		time.Sleep(5 * k.retryAfter)
		if got := hook.heard(); len(got) != 0 {
			t.Fatalf("Deepgram refused a list nobody wants any more and the stream has the wanted one, but the hook heard %v, want nothing", got)
		}
		if got := stream.sent(); len(got) != 1 {
			t.Fatalf("the stream got %q, want no second Configure for the list it has", got)
		}
	})
}

// TestKeytermsRefusalLastsWhileTheWantedListIsMissing fails if the refusal
// hook stops saying a refusal while the stream still lacks the wanted list: a
// newer join's Configure waits, Deepgram refuses it too, or a leave leaves a
// list the stream does not have. Only Deepgram taking the wanted list ends it.
func TestKeytermsRefusalLastsWhileTheWantedListIsMissing(t *testing.T) {
	k, stream := quickKeyterms()
	k.retryAfter = time.Hour
	t.Cleanup(func() { k.Attach(nil, nil) })
	var hook refusalCalls
	k.SetRefusalHook(hook.hook)
	k.SetJoined([]string{"ABC-12"})
	stream.answer(false)
	if !hook.warns() {
		t.Fatalf("control: after a refusal the hook heard %v, want a refusal last", hook.heard())
	}
	k.SetJoined([]string{"ABC-12", "XYZ-9"})
	if got := stream.sent(); len(got) != 2 {
		t.Fatalf("control: a newer join after a refusal sent %q, want a second Configure at once", got)
	}
	if !hook.warns() {
		t.Fatalf("a newer join's Configure waits and the stream still lacks the wanted list, but the hook heard %v", hook.heard())
	}
	stream.answer(false)
	if !hook.warns() {
		t.Fatalf("Deepgram refused the newer list too, but the hook heard %v", hook.heard())
	}
	k.SetJoined([]string{"ABC-12"})
	if got := stream.sent(); len(got) != 3 {
		t.Fatalf("control: a leave to a list the stream lacks sent %q, want a third Configure at once", got)
	}
	if !hook.warns() {
		t.Fatalf("a leave left a list the stream lacks, but the hook heard %v", hook.heard())
	}
	stream.answer(true)
	if hook.warns() {
		t.Fatalf("Deepgram took the wanted list, but the hook heard %v", hook.heard())
	}
}

// TestIsName fails if a word Deepgram writes for the name, as configured or as
// a sound-alike, is not recognised as the name, or another word is.
func TestIsName(t *testing.T) {
	d := New([]string{"Wispr", "Ghost Wispr"})
	for word, want := range map[string]bool{
		"Wispr,": true, "wispr": true, "Whisper.": true, "Ghost": true, "Lispr,": true,
		"Tell": false, "whiskey": false, "Mister,": false, "": false,
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
		// A hundred after "in for" reads as main read it, after any word
		// (Sol's review of #133 at 2949b83).
		"go ahead, in for forty five hundred and two": {"INFRA-4502"},
		"infra forty-five ninety-two":                 {"INFRA-4592"},
		"infra four five nine two":                    {"INFRA-4592"},
		"in for forty five ninety two please":         {"INFRA-4592"},
		"I am in for a minute":                        nil,
		// End-to-end run 8e: smart formatting writes "4,592".
		"ask the agent on in For 4,592 when": {"INFRA-4592"},
		// The office run of 2026-11-21: smart formatting wrote "ten
		// twenty seven" as a date.
		"what's the status of infra 10/27?": {"INFRA-1027"},
		"What's the status of INFRA 10/27?": {"INFRA-1027"},
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
		// The office run of 2026-11-21: smart formatting wrote "ten
		// twenty seven" as a date.
		"what's the status of infra 10/27?": "what's the status of INFRA-1027?",
		"What's the status of INFRA 10/27?": "What's the status of INFRA-1027?",
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
