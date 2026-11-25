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
	now := monoclock.Now()
	t := Transcript{Text: text, IsFinal: final, SpeechFinal: final, GateOpen: true, At: now.Wall, Mono: now.Mono}
	for i, w := range strings.Fields(text) {
		at := start + apart*float64(i)
		t.Words = append(t.Words, Word{Text: w, Speaker: 0, Start: at, End: at + 0.2})
	}
	return t
}

// TestAFinalThatLostTheWakeKeepsOnlyWhatFollowedIt (Sol's reviews of #192 at
// 4ff5250 and 4e52421): Deepgram's final sometimes revises an utterance
// whose interim held the wake phrase, and writes the name otherwise
// ("Mister") or not at all. The turn then held what the final said before
// the wake too, and it went whole to Dispatch's search, its embedding vendor
// and the model. A window in time around the wake's start let the word just
// before it in ("today", "private"). The turn now begins where the words
// that followed the wake in the interim begin in the final; with none of
// them there, at the end of the wake. It fails if the turn holds a word said
// before the wake, or loses one said after it; the controls show a final that
// keeps the wake, and a name the person said after it, untouched.
func TestAFinalThatLostTheWakeKeepsOnlyWhatFollowedIt(t *testing.T) {
	const question = "what is happening with the realtime voice?"
	const several = "Alice left. Bob is away. Carol is not here today. "
	for _, tc := range []struct {
		name, interim, final, want string
		apart, previousShift       float64
	}{
		{"several sentences, the wake kept", "", several + "Wispr, " + question, question, 0.5, 0},
		{"several sentences, the name dropped, slow", several + "Wispr, what is happening", several + question, question, 0.5, 0},
		{"several sentences, Mister, slow", several + "Wispr, what is happening", several + "Mister, " + question, question, 0.5, 0},
		{"several sentences, the name dropped", several + "Wispr, what is happening", several + question, question, 0.2, 0},
		{"several sentences, Mister", several + "Wispr, what is happening", several + "Mister, " + question, question, 0.2, 0},
		{"one sentence, the name dropped", "She is not here today. Wispr, what is happening", "She is not here today. " + question, question, 0.2, 0},
		{"the word before, private", "Her diagnosis is private. Wispr, what is happening", "Her diagnosis is private. " + question, question, 0.2, 0},
		{"the word before moved 150 ms", "Her diagnosis is private. Wispr, what is happening", "Her diagnosis is private. " + question, question, 0.4, 0.15},
		{"so, the wake kept", "", "so Wispr " + question, question, 0.2, 0},
		{"so, the name dropped", "so Wispr what is happening", "so " + question, question, 0.2, 0},
		{"so Mister in place of the wake", "Wispr what is happening", "so Mister " + question, question, 0.2, 0},
		// The words after the wake no longer read as the interim's: the
		// turn begins after the wake's end, and nothing before it is kept.
		{"the words after it revised", "Her diagnosis is private. Wispr, what's going", "Her diagnosis is private. Mister, what is happening with the realtime voice?", "what is happening with the realtime voice?", 0.2, 0},
		// Controls: a name the person said after the wake stays.
		{"Miss Burr after the wake", "", "Wispr, Miss Burr fixed the realtime voice. What changed?", "Miss Burr fixed the realtime voice. What changed?", 0.2, 0},
		{"Mister Lopez after the wake", "", "Wispr, Mister Lopez fixed the realtime voice. What changed?", "Mister Lopez fixed the realtime voice. What changed?", 0.2, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newFakeConcierge()
			r := newRigWith(t, c, func(cfg *SupervisorConfig, _ *rig) { cfg.TurnWait = time.Hour })
			if tc.interim != "" {
				interim := timedWords(tc.interim, 0, tc.apart, false)
				r.s.OnTranscript(&interim)
				r.waitSession()
			}
			final := timedWords(tc.final, 0, tc.apart, true)
			final.SpeechFinal = false
			if tc.previousShift != 0 {
				final.Words[3].Start += tc.previousShift
				final.Words[3].End += tc.previousShift
			}
			r.s.OnTranscript(&final)
			calls := c.heardCalls()
			if len(calls) == 0 {
				t.Fatal("UNKNOWN: the Concierge was told no turn")
			}
			if got := calls[len(calls)-1].words; got != tc.want {
				t.Fatalf("the turn = %q, want %q", got, tc.want)
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
	r.s.OnTranscript(&interim)
	r.waitSession()
	first := timedWords("Wispr, what is happening with the realtime voice?", 0, 0.2, true)
	first.SpeechFinal = false
	r.s.OnTranscript(&first)
	later := timedWords("And what is happening with the wake word?", 5, 0.2, true)
	later.SpeechFinal = false
	r.s.OnTranscript(&later)
	calls := c.heardCalls()
	if len(calls) == 0 {
		t.Fatal("UNKNOWN: the Concierge was told no turn")
	}
	if got, want := calls[len(calls)-1].words, "what is happening with the realtime voice? And what is happening with the wake word?"; got != want {
		t.Fatalf("the turn = %q, want %q", got, want)
	}
}
