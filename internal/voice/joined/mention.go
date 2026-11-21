package joined

import (
	"slices"
	"strings"
	"unicode"

	"github.com/sjawhar/ghost-wispr/internal/wake/transcript"
)

// minTitleWord is the shortest title word that counts toward an issue coming
// up (R17: two words of its title of 5 letters or more).
const minTitleWord = 5

// titleWords are the words of title that count toward its issue coming up:
// 5 letters or more, each once, in lower case.
func titleWords(title string) []string {
	var words []string
	for _, w := range strings.Fields(title) {
		w = word(w)
		if len([]rune(w)) >= minTitleWord && !slices.Contains(words, w) {
			words = append(words, w)
		}
	}
	return words
}

// word is w in lower case with the punctuation around it stripped.
func word(w string) string {
	return strings.ToLower(strings.TrimFunc(w, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }))
}

// comesUp reports whether text, a final segment of the room's, names m's
// issue: its key in any form people say it ("ABC-12", "abc twelve"), its
// number alone when it has 3 digits or more, or two of its title's words.
func (m *member) comesUp(text string) bool {
	said := strings.Fields(text)
	if m.IssueKey != "" {
		if slices.Contains(transcript.IssueKeys(text), m.IssueKey) {
			return true
		}
		if _, number, _ := strings.Cut(m.IssueKey, "-"); len(number) >= 3 && slices.ContainsFunc(said, func(w string) bool { return word(w) == number }) {
			return true
		}
	}
	hits := 0
	for _, title := range m.words {
		if slices.ContainsFunc(said, func(w string) bool { return word(w) == title }) {
			hits++
		}
	}
	return hits >= 2
}

// speakable is title as the voice can name an agent by: its words of
// letters and digits, at most 6.
func speakable(title string) string {
	var words []string
	for _, w := range strings.Fields(title) {
		w = strings.TrimFunc(w, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
		if w != "" && strings.IndexFunc(w, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' && r != '\'' }) < 0 {
			words = append(words, w)
		}
		if len(words) == 6 {
			break
		}
	}
	return strings.Join(words, " ")
}
