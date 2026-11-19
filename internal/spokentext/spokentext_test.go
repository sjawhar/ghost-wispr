package spokentext

import "testing"

const spokenText = "purple elephant on the roof"

// TestQuotesReadsWordsNotLetters: a message quotes the text when it holds the
// text, or two of its words in a row, whatever the case, spacing or
// punctuation between them; not when it only shares a word, or letters.
func TestQuotesReadsWordsNotLetters(t *testing.T) {
	for _, tc := range []struct {
		message, text string
		quotes        bool
	}{
		{`Sentence starts with "purple   ELEPHANT" is too long`, spokenText, true},
		{"bad request for: " + spokenText, spokenText, true},
		{`text "Hello" is not allowed`, "Hello", true},
		{"invalid voice_id", "test", false},
		{"permission denied", "Please read the permission notes.", false},
		{"robotic voice unavailable", spokenText, false},
		{"", spokenText, false},
	} {
		if got := Quotes(tc.message, tc.text); got != tc.quotes {
			t.Errorf("Quotes(%q, %q) = %v, want %v", tc.message, tc.text, got, tc.quotes)
		}
	}
}

// TestWithholdKeepsAMessageThatQuotesNothing fails if a message that quotes
// the text is kept, or one that does not is withheld.
func TestWithholdKeepsAMessageThatQuotesNothing(t *testing.T) {
	if got := Withhold("permission denied", spokenText); got != "permission denied" {
		t.Errorf("Withhold kept %q, want the message", got)
	}
	if got := Withhold("cannot say purple elephant", spokenText); got != Withheld {
		t.Errorf("Withhold kept %q, want it withheld", got)
	}
}
