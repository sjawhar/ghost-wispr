// Package spokentext keeps the text Ghost Wispr speaks out of speech vendors'
// error messages, which end up in API responses and the journal: the text may
// be a meeting's words, such as a summary read out or an answer that quotes
// the room.
package spokentext

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Withheld stands in an error for a vendor's message that quotes the text it
// was asked to speak.
const Withheld = "the provider's message quotes the text to speak, so it is left out"

// Withhold returns message, a vendor's error message or its raw error body,
// for the error of a request to speak text, or Withheld when it quotes text
// (Quotes).
func Withhold(message, text string) string {
	if Quotes(message, text) {
		return Withheld
	}
	return message
}

// Quotes reports whether message holds text, two of its words in a row, or
// one of its names or numbers, compared as words (letters and digits)
// whatever the case, spacing or punctuation. A name is a capitalised word
// that does not start a sentence, as R8 counts names. Another single shared
// word is not a quote: an error such as "permission denied" stays readable
// when the text says "permission".
func Quotes(message, text string) bool {
	words := spokenWords(text)
	said := spokenWords(message)
	if len(words) == 0 || len(said) == 0 {
		return false
	}
	saidSet := make(map[string]bool, len(said))
	for _, word := range said {
		saidSet[word] = true
	}
	if len(words) == 1 {
		return saidSet[words[0]]
	}
	for _, word := range namesAndNumbers(text) {
		if saidSet[word] {
			return true
		}
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

// namesAndNumbers returns, in lower case, the words of text that are names
// (capitalised, not starting a sentence) or hold a digit.
func namesAndNumbers(text string) []string {
	var out []string
	sentenceStart := true
	for _, field := range strings.Fields(text) {
		word := strings.TrimFunc(field, notWordRune)
		if word != "" {
			first, _ := utf8.DecodeRuneInString(word)
			if strings.ContainsFunc(word, unicode.IsDigit) || (unicode.IsUpper(first) && !sentenceStart) {
				out = append(out, spokenWords(word)...)
			}
		}
		sentenceStart = strings.ContainsAny(field[len(field)-1:], ".!?")
	}
	return out
}

func notWordRune(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }

// spokenWords returns the words of s, in lower case.
func spokenWords(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), notWordRune)
}
