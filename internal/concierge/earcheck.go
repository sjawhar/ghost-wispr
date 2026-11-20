package concierge

import "github.com/sjawhar/ghost-wispr/internal/earcheck"

// These local names keep Concierge's non-R8 query helpers on the same tables
// and matchers as the shared R8 checker.
var (
	earKey             = earcheck.Key
	earFileName        = earcheck.FileName
	numberUnits        = earcheck.NumberUnits
	endsInAbbreviation = earcheck.EndsInAbbreviation
)

func set(items []string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, item := range items {
		m[item] = true
	}
	return m
}

// fixedSentence is what the voice says when two rewrites for the ear fail
// the check (R8).
const fixedSentence = "The answer is in writing on the recorder session's page."

// earCheck reports why text fails R8 against source, as a reason that holds
// none of either text's words, or "" when it passes. questionKey is the issue
// key the question named, which the text must hold and which counts as
// source.
func earCheck(text, source, questionKey string) string {
	return earcheck.Check(text, source, questionKey)
}

// earCheckAsked is earCheck for an answer to the question asked, whose own
// words, such as Deepgram's "SliceOne", count as held names: a name the asker
// said is no name the model invented. Its figures and keys do not count.
func earCheckAsked(text, source, asked, questionKey string) string {
	return earcheck.CheckAsked(text, source, asked, questionKey)
}

// earForm reports why text does not have the form the ear takes, or "".
func earForm(text string) string { return earcheck.Form(text) }
