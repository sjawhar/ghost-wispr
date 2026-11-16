package tts

import (
	"strings"
	"unicode"
)

// spokenTextWithheld stands in a TTS error for a provider's message that
// quotes the text it was asked to speak.
const spokenTextWithheld = "the provider's message quotes the text to speak, so it is left out"

// providerMessage returns message, a provider's error message or its raw
// error body, for the error of a request to speak text, or
// spokenTextWithheld when it quotes text (quotesSpokenText). A TTS error is
// kept in the request's status (GET /api/tts/status/{id}) and returned by the
// API, and the text may be a conversation's words, such as a summary read
// out.
func providerMessage(message, text string) string {
	if quotesSpokenText(message, text) {
		return spokenTextWithheld
	}
	return message
}

// quotesSpokenText reports whether message holds text, or two of its words
// in a row, compared as words (letters and digits) whatever the case, spacing
// or punctuation. One shared word is not a quote: an error such as
// "permission denied" stays readable when the text says "permission".
func quotesSpokenText(message, text string) bool {
	words := spokenWords(text)
	said := spokenWords(message)
	if len(words) == 0 || len(said) == 0 {
		return false
	}
	if len(words) == 1 {
		for _, word := range said {
			if word == words[0] {
				return true
			}
		}
		return false
	}
	pairs := make(map[[2]string]bool, len(said))
	for i := 1; i < len(said); i++ {
		pairs[[2]string{said[i-1], said[i]}] = true
	}
	for i := 1; i < len(words); i++ {
		if pairs[[2]string{words[i-1], words[i]}] {
			return true
		}
	}
	return false
}

// spokenWords returns the words of s, in lower case.
func spokenWords(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}
