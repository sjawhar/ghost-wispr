package tts

import "testing"

// TestQuotesSpokenTextReadsWordsNotLetters: a message quotes the text when it
// holds the text, or two of its words in a row, whatever the case, spacing or
// punctuation between them; not when it only shares a word, or letters.
func TestQuotesSpokenTextReadsWordsNotLetters(t *testing.T) {
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
		if got := quotesSpokenText(tc.message, tc.text); got != tc.quotes {
			t.Errorf("quotesSpokenText(%q, %q) = %v, want %v", tc.message, tc.text, got, tc.quotes)
		}
	}
}
