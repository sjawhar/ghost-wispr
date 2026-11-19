package concierge

import (
	"regexp"
	"strconv"
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
	// request 80", "pull request number 80" or a link's "/pull/80".
	earPR = regexp.MustCompile(`(?i)(?:\bPR\s*(?:number\s*)?#?|\bpull\s+request\s*(?:number\s*)?#?|#|/pull/)([0-9]+)\b`)
	// earSentenceEnd ends a sentence, unless an abbreviation's full stop.
	earSentenceEnd = regexp.MustCompile(`[.!?]+(?:\s+|$)`)
	// earAbbreviations are words whose full stop ends no sentence.
	earAbbreviations = set([]string{"e.g", "i.e", "etc", "vs", "cf", "dr", "mr", "mrs", "ms"})
	// numberUnits and numberScales are the words a figure is written in.
	numberUnits = map[string]int{
		"zero": 0, "one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6, "seven": 7, "eight": 8, "nine": 9,
		"ten": 10, "eleven": 11, "twelve": 12, "thirteen": 13, "fourteen": 14, "fifteen": 15, "sixteen": 16,
		"seventeen": 17, "eighteen": 18, "nineteen": 19, "twenty": 20, "thirty": 30, "forty": 40, "fifty": 50,
		"sixty": 60, "seventy": 70, "eighty": 80, "ninety": 90,
	}
	numberScales = map[string]int{"hundred": 100, "thousand": 1000, "million": 1000000}
)

// earCheck reports why text fails R8 against source, as a reason that holds
// none of either text's words, or "" when it passes. questionKey is the issue
// key the question named, which the text must hold and which counts as
// source.
func earCheck(text, source, questionKey string) string {
	if reason := earForm(text); reason != "" {
		return reason
	}
	return earFacts(text, source, questionKey)
}

// earFacts reports why text holds a fact source does not, by R8's rule, or
// "".
func earFacts(text, source, questionKey string) string {
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
	// word: "4.5%" is held by "4.5 percent". A figure in words is its value:
	// "twenty-five" is held by "25", and "25" by "twenty-five".
	figure := func(token string) string { return strings.TrimSuffix(token, "%") }
	sourceTokens := earTokens(allowed)
	known := make(map[string]bool, len(sourceTokens))
	for _, token := range sourceTokens {
		known[figure(token)] = true
	}
	for _, run := range numberRuns(sourceTokens) {
		known[strconv.Itoa(run.value)] = true
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
		tokens := earTokens(clause)
		inRun := make([]bool, len(tokens))
		for _, run := range numberRuns(tokens) {
			if !known[strconv.Itoa(run.value)] {
				return "a figure the source does not hold"
			}
			for i := run.start; i < run.end; i++ {
				inRun[i] = true
			}
		}
		for i, token := range tokens {
			switch {
			case inRun[i]:
				// Checked above.
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

// numberRun is a run of number words, tokens[start:end], and its value.
type numberRun struct{ start, end, value int }

// numberRuns reads the figures tokens write in words: "three", "twenty-five",
// "three hundred". "One" alone is no figure: it is as often a pronoun.
func numberRuns(tokens []string) []numberRun {
	var runs []numberRun
	for i := 0; i < len(tokens); i++ {
		total, current, end := 0, 0, i
		for ; end < len(tokens); end++ {
			next, add, ok := current, 0, true
			for _, part := range strings.Split(strings.ToLower(tokens[end]), "-") {
				if value, unit := numberUnits[part]; unit {
					next += value
					continue
				}
				scale, isScale := numberScales[part]
				if !isScale {
					ok = false
					break
				}
				next = max(next, 1)
				if scale == 100 {
					next *= scale
				} else {
					add, next = add+next*scale, 0
				}
			}
			if !ok {
				break
			}
			total, current = total+add, next
		}
		if end == i {
			continue
		}
		if end > i+1 || !strings.EqualFold(tokens[i], "one") {
			runs = append(runs, numberRun{start: i, end: end, value: total + current})
		}
		i = end - 1
	}
	return runs
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

// earSentences splits text into its sentences. An abbreviation's full stop
// ("e.g.") ends none.
func earSentences(text string) []string {
	var sentences []string
	start := 0
	for _, end := range earSentenceEnd.FindAllStringIndex(text, -1) {
		part := text[start:end[0]]
		word := strings.TrimLeft(part[strings.LastIndexFunc(part, unicode.IsSpace)+1:], `("'`)
		if text[end[0]] == '.' && earAbbreviations[strings.ToLower(word)] {
			continue
		}
		if strings.TrimSpace(part) != "" {
			sentences = append(sentences, part)
		}
		start = end[1]
	}
	if rest := text[start:]; strings.TrimSpace(rest) != "" {
		sentences = append(sentences, rest)
	}
	return sentences
}

// earClauses splits text into the parts whose first word is no name for
// being capitalised: its sentences, and a sentence's clause after a leading
// issue key ("ABC-12: Done."), which is how the question's key is put in
// front. A clause after any other colon is no such part.
func earClauses(text string) []string {
	var clauses []string
	for _, sentence := range earSentences(text) {
		trimmed := strings.TrimSpace(sentence)
		if key := earKey.FindString(trimmed); key != "" && strings.HasPrefix(trimmed, key+": ") {
			clauses = append(clauses, key, trimmed[len(key)+2:])
			continue
		}
		clauses = append(clauses, sentence)
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
