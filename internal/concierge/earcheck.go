package concierge

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// R8's check of a text written for the ear against its source: the agent's
// written answer, or the Dispatch record. It holds no fact the source does
// not, and it has the form the ear takes.

// fixedSentence is what the voice says when two rewrites for the ear fail
// the check (R8).
const fixedSentence = "The answer is in writing on the recorder session's page."

const (
	maxEarSentences   = 3
	maxEarWords       = 50
	maxSentenceWords  = 20
	longIdentifier    = 12 // an identifier this long or longer may be spoken cut
	cutIdentifierSize = 7  // to its first this many characters
)

var (
	// earKey is an issue key as R8 knows it.
	earKey = regexp.MustCompile(`\b[A-Z][A-Z0-9]*-[0-9]+\b`)
	// earPR is a pull request number: "#80", "PR 80", "PR #80", "pull
	// request 80" or a link's "/pull/80".
	earPR = regexp.MustCompile(`(?i)(?:\bPR\s*#?|\bpull\s+request\s*#?|#|/pull/)([0-9]+)\b`)
	// earSentenceEnd ends a sentence.
	earSentenceEnd = regexp.MustCompile(`[.!?]+(?:\s+|$)`)
)

// earCheck reports why text fails R8 against source, as a reason that holds
// none of either text's words, or "" when it passes. questionKey is the issue
// key the question named, which the text must hold and which counts as
// source.
func earCheck(text, source, questionKey string) string {
	if reason := earForm(text); reason != "" {
		return reason
	}
	allowed := source + "\n" + questionKey
	sourceKeys := set(earKey.FindAllString(allowed, -1))
	textKeys := earKey.FindAllString(text, -1)
	for _, key := range textKeys {
		if !sourceKeys[key] {
			return "an issue key the source does not hold"
		}
	}
	if questionKey != "" && !set(textKeys)[questionKey] {
		return "the question's issue key is missing"
	}
	sourcePRs := set(prNumbers(source))
	for _, number := range prNumbers(text) {
		if !sourcePRs[number] {
			return "a pull request number the source does not hold"
		}
	}
	// A figure is compared without a percent sign, which the ear says as a
	// word: "4.5%" is held by "4.5 percent".
	figure := func(token string) string { return strings.TrimSuffix(token, "%") }
	sourceTokens := earTokens(allowed)
	known := make(map[string]bool, len(sourceTokens))
	for _, token := range sourceTokens {
		known[figure(token)] = true
	}
	held := func(token string) bool {
		if known[figure(token)] {
			return true
		}
		if len(token) == cutIdentifierSize {
			for _, s := range sourceTokens {
				if len(s) >= longIdentifier && strings.HasPrefix(s, token) {
					return true
				}
			}
		}
		return false
	}
	for _, clause := range earClauses(text) {
		for i, token := range earTokens(clause) {
			switch {
			case earKey.MatchString(token) && earKey.FindString(token) == token:
				// Checked above.
			case strings.IndexFunc(token, unicode.IsDigit) >= 0:
				if !held(token) {
					return "a figure the source does not hold"
				}
			case i > 0 && isName(token):
				if !held(token) {
					return "a name the source does not hold"
				}
			}
		}
	}
	return ""
}

// earForm reports why text does not have the form the ear takes, or "".
func earForm(text string) string {
	if strings.TrimSpace(text) == "" {
		return "no text"
	}
	if strings.ContainsAny(text, "`|") {
		return "a backtick or a bar"
	}
	for _, word := range strings.Fields(text) {
		if strings.Contains(strings.Trim(word, `.,;:!?()"'`), "/") {
			return "a slash inside a word"
		}
	}
	sentences := earSentences(text)
	if len(sentences) > maxEarSentences {
		return "more than 3 sentences"
	}
	if len(strings.Fields(text)) > maxEarWords {
		return "more than 50 words"
	}
	for _, sentence := range sentences {
		if len(strings.Fields(sentence)) > maxSentenceWords {
			return "a sentence of more than 20 words"
		}
	}
	return ""
}

// earSentences splits text into its sentences.
func earSentences(text string) []string {
	var sentences []string
	for _, part := range earSentenceEnd.Split(text, -1) {
		if strings.TrimSpace(part) != "" {
			sentences = append(sentences, part)
		}
	}
	return sentences
}

// earClauses splits text into the parts whose first word is no name for
// being capitalised: its sentences, and the clauses after a colon ("ABC-12:
// Done.").
func earClauses(text string) []string {
	var clauses []string
	for _, sentence := range earSentences(text) {
		for _, clause := range strings.Split(sentence, ": ") {
			if strings.TrimSpace(clause) != "" {
				clauses = append(clauses, clause)
			}
		}
	}
	return clauses
}

// earTokens is text's words, with the punctuation around each trimmed and a
// possessive's "'s" dropped. A token keeps its inner ".", ":", "%", "-" and
// "'" ("08:10", "4.5%", "ABC-12").
func earTokens(text string) []string {
	fields := strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && !strings.ContainsRune(".:%-'", r)
	})
	tokens := fields[:0]
	for _, field := range fields {
		field = strings.TrimSuffix(strings.Trim(field, ".:-'"), "'s")
		if field != "" {
			tokens = append(tokens, field)
		}
	}
	return tokens
}

// isName reports whether token is a name by R8's rule: a capitalised word,
// other than "I" and the "PR" of a pull request number.
func isName(token string) bool {
	first, _ := utf8.DecodeRuneInString(token)
	return unicode.IsUpper(first) && token != "I" && token != "PR"
}

// prNumbers lists the pull request numbers text names.
func prNumbers(text string) []string {
	var numbers []string
	for _, match := range earPR.FindAllStringSubmatch(text, -1) {
		numbers = append(numbers, match[1])
	}
	return numbers
}

func set(items []string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, item := range items {
		m[item] = true
	}
	return m
}
