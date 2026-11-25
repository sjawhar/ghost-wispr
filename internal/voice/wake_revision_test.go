package voice

import (
	"strings"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/monoclock"
)

// timedWords is a transcript of text whose words are apart seconds, the first
// at start: a room's words have 0.3 s or more between them, and the words
// before a wake phrase must be far enough from it to be told apart (the
// final may move the wake by finalMoves).
func timedWords(text string, start, apart float64, final bool) Transcript {
	now := monoclock.Now()
	t := Transcript{Text: text, IsFinal: final, SpeechFinal: final, GateOpen: true, At: now.Wall, Mono: now.Mono}
	for i, w := range strings.Fields(text) {
		at := start + apart*float64(i)
		t.Words = append(t.Words, Word{Text: w, Speaker: 0, Start: at, End: at + 0.2})
	}
	return t
}

// TestAFinalThatDroppedTheNameKeepsOnlyWhatFollowedIt (Sol's review of #192):
// Deepgram's final sometimes revises an utterance whose interim held the wake
// phrase, and writes the name differently ("Mister") or not at all. The turn
// then held everything the final said, the speech before the wake included,
// and that speech went whole to Dispatch's search and its embedding vendor.
// It fails if the turn the Concierge is told holds a word said before the
// wake phrase, or loses a word said after it.
func TestAFinalThatDroppedTheNameKeepsOnlyWhatFollowedIt(t *testing.T) {
	const question = "what is happening with the realtime voice?"
	for _, tc := range []struct{ name, final, want string }{
		{"the name is dropped", "She is not here today. " + question, question},
		{"a person's address in its place", "She is not here today. Mister, " + question, "Mister, " + question},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newFakeConcierge("ABC-12 is in progress.")
			r := newRig(t, c)
			// The wake phrase is said 2.5 s in, after a sentence of five
			// words the room said to each other.
			interim := timedWords("Wispr, "+question, 2.5, 0.5, false)
			r.s.OnTranscript(&interim)
			r.waitSession()
			final := timedWords(tc.final, 0, 0.5, true)
			r.s.OnTranscript(&final)
			got := r.waitTurn()
			if got != tc.want {
				t.Fatalf("the turn = %q, want %q", got, tc.want)
			}
			for _, h := range c.heardCalls() {
				if strings.Contains(h.words, "here today") {
					t.Fatalf("the Concierge read for %q, which holds words said before the wake phrase", h.words)
				}
			}
		})
	}
}
