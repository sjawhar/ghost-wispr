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
		"what is happening with this?": Ask,
		"":                             Ask,
	}
	for rest, want := range cases {
		if got := CommandOf(rest); got != want {
			t.Errorf("CommandOf(%q) = %v, want %v", rest, got, want)
		}
	}
}

func TestKeytermOrderAndBudget(t *testing.T) {
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
