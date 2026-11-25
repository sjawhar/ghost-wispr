package voice

import (
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/monoclock"
)

// timedWords is a transcript of text whose words are apart seconds, the first
// at start, each 0.2 s long.
func timedWords(text string, start, apart float64, final bool) Transcript {
	return timedWordsOf(text, start, apart, 0.2, final)
}

// timedWordsOf is timedWords with words long seconds long.
func timedWordsOf(text string, start, apart, long float64, final bool) Transcript {
	now := monoclock.Now()
	t := Transcript{Text: text, IsFinal: final, SpeechFinal: final, GateOpen: true, At: now.Wall, Mono: now.Mono}
	for i, w := range strings.Fields(text) {
		at := start + apart*float64(i)
		t.Words = append(t.Words, Word{Text: w, Speaker: 0, Start: at, End: at + long})
	}
	return t
}

// heardTurn feeds r an interim, when there is one, then the finals, none of
// them speech_final, and is the turn the Concierge was last told.
func heardTurn(t *testing.T, r *rig, c *fakeConcierge, interim *Transcript, finals ...Transcript) string {
	t.Helper()
	if interim != nil {
		r.s.OnTranscript(interim)
		r.waitSession()
	}
	for i := range finals {
		finals[i].SpeechFinal = false
		r.s.OnTranscript(&finals[i])
	}
	calls := c.heardCalls()
	if len(calls) == 0 {
		t.Fatal("UNKNOWN: the Concierge was told no turn")
	}
	return calls[len(calls)-1].words
}

// TestATurnThatLostItsWakeIsTheInterimsWordsAndWhatFollowed (Sol's three
// reviews of #192): Deepgram's final sometimes revises an utterance whose
// interim held the wake phrase, and writes the name otherwise ("Mister") or
// not at all. The turn held what the final said before the wake, which went
// to Dispatch's search, its embedding vendor and the model; a window in time
// around the wake, then words matched in the final, each let a word from
// before it in. The turn is now the interim's own words after its wake, then
// the final's words that start after the interim's last word ends; with no
// word after the wake in the interim, the final's words from the wake's end
// on. No word from before the wake can enter, and no window or matching is
// needed. The cost, in the fail-safe direction: the final's corrections of
// those words are lost ("Alex" for "Alice"), and a final whose word times
// moved earlier loses the word that moved before the interim's end, which
// these fixtures cause by timing words by their place in the text, so that
// a dropped wake moves every later word one place earlier; a final whose
// words moved later repeats the one that moved past it. Sol's fixtures,
// every round's, as Sol wrote them. It fails if the turn holds a word said
// before the wake, or differs from what the rule gives.
func TestATurnThatLostItsWakeIsTheInterimsWordsAndWhatFollowed(t *testing.T) {
	const q = "what is happening with the realtime voice?"
	const several = "Alice left. Bob is away. Carol is not here today. "
	type part struct {
		text  string
		start float64
	}
	for _, tc := range []struct {
		name, interim, want string
		apart, long, start  float64
		// moved is how many seconds the final's word i moved from the time
		// its place in the text gives it; nil moves none.
		moved  func(i int) float64
		finals []part
		before []string // words said before the wake, which must not be in the turn
	}{
		// Round 1 and round 2.
		{name: "several sentences, the wake kept", want: q, apart: .5, long: .2, finals: []part{{several + "Wispr, " + q, 0}}, before: []string{"today"}},
		{name: "several sentences, the name dropped, 0.5 s", interim: several + "Wispr, what is happening", want: "what is happening the realtime voice?", apart: .5, long: .2, finals: []part{{several + q, 0}}, before: []string{"today"}},
		{name: "several sentences, Mister, 0.5 s", interim: several + "Wispr, what is happening", want: q, apart: .5, long: .2, finals: []part{{several + "Mister, " + q, 0}}, before: []string{"today", "Mister"}},
		{name: "several sentences, the name dropped, 0.2 s", interim: several + "Wispr, what is happening", want: "what is happening the realtime voice?", apart: .2, long: .2, finals: []part{{several + q, 0}}, before: []string{"today"}},
		{name: "several sentences, Mister, 0.2 s", interim: several + "Wispr, what is happening", want: q, apart: .2, long: .2, finals: []part{{several + "Mister, " + q, 0}}, before: []string{"today", "Mister"}},
		{name: "one sentence, the name dropped", interim: "She is not here today. Wispr, what is happening", want: "what is happening the realtime voice?", apart: .2, long: .2, finals: []part{{"She is not here today. " + q, 0}}, before: []string{"today"}},
		{name: "the word before, private", interim: "Her diagnosis is private. Wispr, what is happening", want: "what is happening the realtime voice?", apart: .2, long: .2, finals: []part{{"Her diagnosis is private. " + q, 0}}, before: []string{"private"}},
		{name: "private moved 150 ms", interim: "Her diagnosis is private. Wispr, what is happening", want: "what is happening the realtime voice?", apart: .4, long: .2, moved: func(i int) float64 { return map[int]float64{3: .15}[i] }, finals: []part{{"Her diagnosis is private. " + q, 0}}, before: []string{"private"}},
		{name: "so, the wake kept", want: q, apart: .2, long: .2, finals: []part{{"so Wispr " + q, 0}}, before: []string{"so "}},
		{name: "so, the name dropped", interim: "so Wispr what is happening", want: "what is happening the realtime voice?", apart: .2, long: .2, finals: []part{{"so " + q, 0}}, before: []string{"so "}},
		{name: "so Mister in place of the wake", interim: "Wispr what is happening", want: "what is happening happening with the realtime voice?", apart: .2, long: .2, finals: []part{{"so Mister " + q, 0}}, before: []string{"so ", "Mister"}},
		{name: "the words after it revised", interim: "Her diagnosis is private. Wispr, what's going", want: "what's going happening with the realtime voice?", apart: .2, long: .2, finals: []part{{"Her diagnosis is private. Mister, what is happening with the realtime voice?", 0}}, before: []string{"private", "Mister"}},
		{name: "control: Miss Burr after the wake", want: "Miss Burr fixed the realtime voice. What changed?", apart: .2, long: .2, finals: []part{{"Wispr, Miss Burr fixed the realtime voice. What changed?", 0}}},
		{name: "control: Mister Lopez after the wake", want: "Mister Lopez fixed the realtime voice. What changed?", apart: .2, long: .2, finals: []part{{"Wispr, Mister Lopez fixed the realtime voice. What changed?", 0}}},
		// Round 3.
		{name: "repeated three, 200 ms", interim: "what is happening? Wispr, what is happening", want: "what is happening the realtime voice?", apart: .2, long: .15, finals: []part{{"what is happening? " + q, 0}}, before: []string{"happening? "}},
		{name: "repeated three, 100 ms", interim: "what is happening? Wispr, what is happening", want: "what is happening the realtime voice?", apart: .1, long: .08, start: 10, finals: []part{{"what is happening? " + q, 10}}, before: []string{"happening? "}},
		// The three words before the wake moved 300 ms toward it, the rest
		// 200 ms later.
		{name: "repeated three, 200 ms, moved 300 ms", interim: "what is happening? Wispr, what is happening", want: q, apart: .2, long: .08, moved: func(i int) float64 { return map[bool]float64{true: .3, false: .2}[i < 3] }, finals: []part{{"what is happening? " + q, 0}}, before: []string{"happening? "}},
		{name: "changed one, Mister", interim: "Her diagnosis is private. Wispr, Alice is waiting", want: "Alice is waiting for the realtime voice?", apart: .2, long: .15, finals: []part{{"Her diagnosis is private. Mister, Alex is waiting for the realtime voice?", 0}}, before: []string{"private", "Mister"}},
		{name: "changed one, dropped", interim: "Her diagnosis is private. Wispr, Alice is waiting", want: "Alice is waiting the realtime voice?", apart: .2, long: .15, finals: []part{{"Her diagnosis is private. Alex is waiting for the realtime voice?", 0}}, before: []string{"private"}},
		{name: "short, none after the wake", interim: "Her diagnosis is private. Wispr,", want: q, apart: .2, long: .15, finals: []part{{"Her diagnosis is private. Mister, " + q, 0}}, before: []string{"private", "Mister"}},
		{name: "short, one after the wake", interim: "Her diagnosis is private. Wispr, what", want: "what happening with the realtime voice?", apart: .2, long: .15, finals: []part{{"Her diagnosis is private. " + q, 0}}, before: []string{"private"}},
		{name: "short, two after the wake", interim: "Her diagnosis is private. Wispr, what is", want: "what is with the realtime voice?", apart: .2, long: .15, finals: []part{{"Her diagnosis is private. " + q, 0}}, before: []string{"private"}},
		{name: "short, one, repeated", interim: "The rollout is private. Wispr, private", want: "private rollout status?", apart: .2, long: .15, finals: []part{{"The rollout is private. private cloud rollout status?", 0}}, before: []string{"private."}},
		// The four words before the wake moved 150 ms toward it.
		{name: "short, two, repeated, moved 150 ms", interim: "We know private cloud. Wispr, private cloud", want: "private cloud status?", apart: .2, long: .15, moved: func(i int) float64 { return map[bool]float64{true: .15}[i < 4] }, finals: []part{{"We know private cloud. private cloud rollout status?", 0}}, before: []string{"cloud."}},
		{name: "split before the anchor", interim: "Her diagnosis is private. Wispr, what is happening", want: "what is happening the realtime voice?", apart: .2, long: .15, finals: []part{{"Her diagnosis is private.", 0}, {q, .8}}, before: []string{"private"}},
		{name: "split inside the anchor", interim: "Her diagnosis is private. Wispr, Alice is waiting", want: "Alice is waiting the realtime voice?", apart: .2, long: .15, finals: []part{{"Her diagnosis is private. Alice is", 0}, {"waiting for the realtime voice?", 1.2}}, before: []string{"private"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newFakeConcierge()
			r := newRigWith(t, c, func(cfg *SupervisorConfig, _ *rig) { cfg.TurnWait = time.Hour })
			var interim *Transcript
			if tc.interim != "" {
				it := timedWordsOf(tc.interim, tc.start, tc.apart, tc.long, false)
				interim = &it
			}
			finals := make([]Transcript, 0, len(tc.finals))
			for _, p := range tc.finals {
				f := timedWordsOf(p.text, p.start, tc.apart, tc.long, true)
				for i := range f.Words {
					if tc.moved != nil {
						f.Words[i].Start += tc.moved(i)
						f.Words[i].End += tc.moved(i)
					}
				}
				finals = append(finals, f)
			}
			got := heardTurn(t, r, c, interim, finals...)
			for _, before := range tc.before {
				if strings.Contains(" "+got+" ", " "+before) {
					t.Errorf("the turn %q holds %q, said before the wake", got, before)
				}
			}
			if got != tc.want {
				t.Errorf("the turn = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestATurnThatLostItsWakeLosesNoWordWhenTimesFollowTheAudio: Deepgram
// times each word by where it was said, so a final that drops the wake or
// writes "Mister" for it leaves the other words where they were. It fails
// if such a turn holds a word said before the wake, or loses one said after
// it.
func TestATurnThatLostItsWakeLosesNoWordWhenTimesFollowTheAudio(t *testing.T) {
	const q = "what is happening with the realtime voice?"
	for _, tc := range []struct {
		name, before, after string
		as                  string // the final's word in place of the wake; "" drops it
		apart               float64
	}{
		{"several sentences, the name dropped", "Alice left. Bob is away. Carol is not here today.", "what is happening", "", .2},
		{"several sentences, Mister", "Alice left. Bob is away. Carol is not here today.", "what is happening", "Mister,", .2},
		{"private, the name dropped, 0.5 s", "Her diagnosis is private.", "what is happening", "", .5},
		{"so, the name dropped", "so", "what is happening", "", .2},
		{"none after the wake", "Her diagnosis is private.", "", "", .2},
		{"one after the wake, repeated", "The rollout is private.", "private", "", .2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newFakeConcierge()
			r := newRigWith(t, c, func(cfg *SupervisorConfig, _ *rig) { cfg.TurnWait = time.Hour })
			wake := len(strings.Fields(tc.before))
			interim := timedWords(strings.TrimSpace(tc.before+" Wispr, "+tc.after), 0, tc.apart, false)
			final := timedWords(tc.before+" Wispr, "+q, 0, tc.apart, true)
			if tc.as == "" {
				final.Words = append(final.Words[:wake:wake], final.Words[wake+1:]...)
			} else {
				final.Words[wake].Text = tc.as
			}
			texts := make([]string, len(final.Words))
			for i, w := range final.Words {
				texts[i] = w.Text
			}
			final.Text = strings.Join(texts, " ")
			want := q
			if strings.Fields(tc.after) != nil && !strings.HasPrefix(q, tc.after) {
				want = tc.after + " " + strings.Join(strings.Fields(q)[len(strings.Fields(tc.after)):], " ")
			}
			if got := heardTurn(t, r, c, &interim, final); got != want {
				t.Fatalf("the turn = %q, want %q", got, want)
			}
		})
	}
}

// TestALaterResultOfTheTurnIsKeptWhole: a result said after the wake, in a
// later segment of the same turn, holds no word from before it, and is kept
// whole, though its words repeat the ones that followed the wake. It fails
// if it loses a word.
func TestALaterResultOfTheTurnIsKeptWhole(t *testing.T) {
	c := newFakeConcierge()
	r := newRigWith(t, c, func(cfg *SupervisorConfig, _ *rig) { cfg.TurnWait = time.Hour })
	interim := timedWords("Wispr, what is happening", 0, 0.2, false)
	first := timedWords("Wispr, what is happening with the realtime voice?", 0, 0.2, true)
	later := timedWords("And what is happening with the wake word?", 5, 0.2, true)
	if got, want := heardTurn(t, r, c, &interim, first, later), "what is happening with the realtime voice? And what is happening with the wake word?"; got != want {
		t.Fatalf("the turn = %q, want %q", got, want)
	}
}
