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
	earAbbreviations = set([]string{"e.g", "i.e", "vs", "cf", "dr", "mr", "mrs", "ms"})
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
	return earCheckAsked(text, source, "", questionKey)
}

// earCheckAsked is earCheck for an answer to the question asked, whose own
// words, such as Deepgram's "SliceOne", count as held names: a name the asker
// said is no name the model invented. Its figures and keys do not count.
func earCheckAsked(text, source, asked, questionKey string) string {
	if reason := earForm(text); reason != "" {
		return reason
	}
	return earFacts(text, source, asked, questionKey)
}

// earFacts reports why text holds a fact source does not, by R8's rule, or
// "". A name in asked is held.
func earFacts(text, source, asked, questionKey string) string {
	askedNames := set(earTokens(asked))
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
		// "4-hour" holds 4 and "5:30" holds 5 and 30, but a key's number
		// is no figure.
		if strings.ContainsAny(token, "-:") && earKey.FindString(token) != token {
			for _, part := range strings.FieldsFunc(token, func(r rune) bool { return r == '-' || r == ':' }) {
				known[figure(part)] = true
			}
		}
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
			// A year-shaped run ("ten thirty") is also a clock time or a
			// list ("12, 13"): it is held either way.
			asParts := len(run.parts) == 2 && known[strconv.Itoa(run.parts[0])] && known[strconv.Itoa(run.parts[1])]
			if !known[strconv.Itoa(run.value)] && !asParts {
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
				if !held(token) && !askedNames[token] {
					return "a name the source does not hold"
				}
			}
		}
	}
	return ""
}

// numberRun is a run of number words, tokens[start:end], and its value.
// parts, for a run read as a year ("twenty twenty-six"), are its two halves,
// as a clock time or a list would read them.
type numberRun struct {
	start, end, value int
	parts             []int
}

// numberKind is what a number word does in a figure.
type numberKind int

const (
	kindNone numberKind = iota
	kindUnit
	kindTeen
	kindTens
	kindHundred
	kindBig
)

// numberWord is one number word's value and kind.
type numberWord struct {
	value int
	kind  numberKind
}

// numberWordsOf reads token's leading number words, split at its hyphens:
// "twenty-five" is 20 and 5, "three-hour" is 3 with a rest (partial).
func numberWordsOf(token string) (words []numberWord, partial bool) {
	parts := strings.Split(strings.ToLower(token), "-")
	for _, part := range parts {
		value, unit := numberUnits[part]
		scale, isScale := numberScales[part]
		switch {
		case unit && value < 10:
			words = append(words, numberWord{value, kindUnit})
		case unit && value < 20:
			words = append(words, numberWord{value, kindTeen})
		case unit:
			words = append(words, numberWord{value, kindTens})
		case isScale && scale == 100:
			words = append(words, numberWord{scale, kindHundred})
		case isScale:
			words = append(words, numberWord{scale, kindBig})
		default:
			return words, len(words) > 0
		}
	}
	return words, false
}

// figureWords adds up one figure's words.
type figureWords struct {
	total, current, words int
	first, last           numberWord
}

// accepts reports whether word goes on the figure: "twenty five" is one
// figure, "seven eight" and "five thirty" are two.
func (f *figureWords) accepts(word numberWord) bool {
	switch {
	case f.words == 0:
		return true
	case word.kind == kindHundred || word.kind == kindBig:
		return true
	case f.last.kind == kindTens:
		return word.kind == kindUnit
	case f.last.kind == kindHundred || f.last.kind == kindBig:
		return word.kind == kindUnit || word.kind == kindTeen || word.kind == kindTens
	}
	return false
}

func (f *figureWords) add(word numberWord) {
	switch word.kind {
	case kindHundred:
		f.current = max(f.current, 1) * word.value
	case kindBig:
		f.total, f.current = f.total+max(f.current, 1)*word.value, 0
	default:
		f.current += word.value
	}
	if f.words == 0 {
		f.first = word
	}
	f.last = word
	f.words++
}

func (f *figureWords) value() int { return f.total + f.current }

// numberRuns reads the figures tokens write in words: "three", "twenty-five",
// "three hundred", "three-hour", and a year as said, "twenty twenty-six".
// "One" alone is no figure: it is as often a pronoun ("one of them").
func numberRuns(tokens []string) []numberRun {
	var runs []numberRun
	for i := 0; i < len(tokens); {
		var figure figureWords
		var parts []int
		end := i
	figureTokens:
		for end < len(tokens) {
			words, partial := numberWordsOf(tokens[end])
			if len(words) == 0 {
				break
			}
			if !figure.accepts(words[0]) {
				// A year as said: "twenty twenty-six" is 2026.
				if figure.words == 1 && (figure.first.kind == kindTeen || figure.first.kind == kindTens) &&
					(words[0].kind == kindTens || words[0].kind == kindTeen) && !partial {
					var rest figureWords
					for _, word := range words {
						rest.add(word)
					}
					parts = []int{figure.first.value, rest.value()}
					figure.current = figure.first.value*100 + rest.value()
					end++
				}
				break
			}
			for _, word := range words {
				if !figure.accepts(word) {
					break figureTokens
				}
				figure.add(word)
			}
			end++
			if partial {
				break
			}
		}
		if end == i {
			i++
			continue
		}
		if end > i+1 || figure.value() != 1 {
			runs = append(runs, numberRun{start: i, end: end, value: figure.value(), parts: parts})
		}
		i = end
	}
	return runs
}

// earFileName is a file's name, which the ear hears as "spec dot md".
// ".js" and ".ts" are left out: "Node.js" and "Next.js" are names.
var earFileName = regexp.MustCompile(`(?i)\b[\w-]+\.(?:md|go|txt|json|ya?ml|toml|sh|py|sql|html|css|log)\b`)

// earForm reports why text does not have the form the ear takes, or "".
func earForm(text string) string {
	if strings.TrimSpace(text) == "" {
		return "no text"
	}
	if strings.ContainsAny(text, "`|") {
		return "a backtick or a bar"
	}
	if earFileName.MatchString(text) {
		return "a file name"
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
		if text[end[0]] == '.' && endsInAbbreviation(part) {
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

// endsInAbbreviation reports whether text, cut just before a full stop, ends
// in an abbreviation whose full stop ends no sentence ("e.g").
func endsInAbbreviation(text string) bool {
	word := strings.TrimLeft(text[strings.LastIndexFunc(text, unicode.IsSpace)+1:], `("'`)
	return earAbbreviations[strings.ToLower(word)]
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
