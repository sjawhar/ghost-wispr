package joined

import (
	"slices"
	"strings"
	"unicode"

	"github.com/sjawhar/ghost-wispr/internal/wake/transcript"
)

// minTitleWord is the shortest title word that counts toward an issue coming
// up: shorter words, such as "the" or "keys", are said all the time.
const minTitleWord = 5

// commonWords are words of 5 letters or more that say nothing of an issue, so
// a title's copy of one does not count toward it coming up.
var commonWords = map[string]bool{
	"about": true, "above": true, "after": true, "again": true, "against": true, "along": true, "among": true,
	"around": true, "because": true, "before": true, "being": true, "below": true, "between": true,
	"could": true, "doing": true, "during": true, "every": true, "first": true, "going": true, "having": true,
	"maybe": true, "might": true, "never": true, "often": true, "other": true, "really": true, "rather": true,
	"should": true, "since": true, "still": true, "their": true, "there": true, "these": true, "thing": true,
	"things": true, "think": true, "those": true, "through": true, "today": true, "under": true, "until": true,
	"where": true, "which": true, "while": true, "whose": true, "within": true, "without": true, "would": true,
}

// titleWords are the words of title that count toward its issue coming up:
// 5 letters or more, not a common word, each once, in lower case.
func titleWords(title string) []string {
	var words []string
	for _, w := range strings.Fields(title) {
		w = word(w)
		if len([]rune(w)) >= minTitleWord && !commonWords[w] && !slices.Contains(words, w) {
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
		if slices.ContainsFunc(said, func(w string) bool { return sameWord(word(w), title) }) {
			hits++
		}
	}
	return hits >= 2
}

// sameWord reports whether a said word is a title word, or its plural or
// singular ("agent" for "agents").
func sameWord(said, title string) bool {
	return said == title || said == title+"s" || said+"s" == title
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
