// Package earcheck verifies text intended for the ear against written source.
package earcheck

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/sjawhar/ghost-wispr/internal/wake/transcript"
)

const (
	maxEarSentences   = 3
	maxEarWords       = 50
	maxSentenceWords  = 20
	longIdentifier    = 12
	cutIdentifierSize = 7
)

var (
	earKey           = regexp.MustCompile(`\b[A-Z][A-Z0-9]*-[0-9]+\b`)
	earPR            = regexp.MustCompile(`(?i)(?:\bPR\s*(?:number\s*)?#?|\bpull\s+request\s*(?:number\s*)?#?|#|/pull/)([0-9]+)\b`)
	earSentenceEnd   = regexp.MustCompile(`[.!?]+(?:\s+|$)`)
	earAbbreviations = set([]string{"e.g", "i.e", "vs", "cf", "dr", "mr", "mrs", "ms"})
	numberUnits      = map[string]int{
		"zero": 0, "one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6, "seven": 7, "eight": 8, "nine": 9,
		"ten": 10, "eleven": 11, "twelve": 12, "thirteen": 13, "fourteen": 14, "fifteen": 15, "sixteen": 16,
		"seventeen": 17, "eighteen": 18, "nineteen": 19, "twenty": 20, "thirty": 30, "forty": 40, "fifty": 50,
		"sixty": 60, "seventy": 70, "eighty": 80, "ninety": 90,
	}
	numberScales = map[string]int{"hundred": 100, "thousand": 1000, "million": 1000000}
	earFileName  = regexp.MustCompile(`(?i)\b[\w-]+\.(?:md|go|txt|json|ya?ml|toml|sh|py|sql|html|css|log)\b`)
)

// Key is R8's issue-key matcher. Concierge uses it when it prefixes a
// rewrite with the question's key.
var Key = earKey

// NumberUnits is the R8 word-to-figure table. Concierge uses it while it
// parses spoken question terms.
var NumberUnits = numberUnits

// FileName is the R8 matcher for a file name that is unsuitable to say.
var FileName = earFileName

// QuestionKey finds the last key the asker named, using the same spoken-key
// forms as the Supervisor: `infra 1027` and `infra ten twenty-seven` both
// become `INFRA-1027`.
func QuestionKey(text string) string {
	keys := transcript.IssueKeys(text)
	if len(keys) == 0 {
		return ""
	}
	return keys[len(keys)-1]
}

// Check reports why text fails the R8 ear contract against source, or "".
func Check(text, source, questionKey string) string { return CheckAsked(text, source, "", questionKey) }

// CheckAsked additionally holds names in asked as permitted source names.
func CheckAsked(text, source, asked, questionKey string) string {
	if reason := Form(text); reason != "" {
		return reason
	}
	return facts(text, source, asked, questionKey)
}

func facts(text, source, asked, questionKey string) string {
	askedNames := set(tokens(asked))
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
	figure := func(token string) string { return strings.TrimSuffix(token, "%") }
	sourceTokens := tokens(allowed)
	known := make(map[string]bool, len(sourceTokens))
	for _, token := range sourceTokens {
		known[figure(token)] = true
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
	for _, clause := range clauses(text) {
		toks := tokens(clause)
		inRun := make([]bool, len(toks))
		for _, run := range numberRuns(toks) {
			asParts := len(run.parts) == 2 && known[strconv.Itoa(run.parts[0])] && known[strconv.Itoa(run.parts[1])]
			if !known[strconv.Itoa(run.value)] && !asParts {
				return "a figure the source does not hold"
			}
			for i := run.start; i < run.end; i++ {
				inRun[i] = true
			}
		}
		for i, token := range toks {
			switch {
			case inRun[i]:
			case earKey.MatchString(token) && earKey.FindString(token) == token:
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

type numberRun struct {
	start, end, value int
	parts             []int
}
type numberKind int

const (
	kindNone numberKind = iota
	kindUnit
	kindTeen
	kindTens
	kindHundred
	kindBig
)

type numberWord struct {
	value int
	kind  numberKind
}

func numberWordsOf(token string) (words []numberWord, partial bool) {
	for _, part := range strings.Split(strings.ToLower(token), "-") {
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

type figureWords struct {
	total, current, words int
	first, last           numberWord
}

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

func numberRuns(toks []string) []numberRun {
	var runs []numberRun
	for i := 0; i < len(toks); {
		var figure figureWords
		var parts []int
		end := i
	figureTokens:
		for end < len(toks) {
			words, partial := numberWordsOf(toks[end])
			if len(words) == 0 {
				break
			}
			if !figure.accepts(words[0]) {
				if figure.words == 1 && (figure.first.kind == kindTeen || figure.first.kind == kindTens) && (words[0].kind == kindTens || words[0].kind == kindTeen) && !partial {
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

// Form reports why text does not have the form the ear takes, or "".
func Form(text string) string {
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
	sents := sentenceTexts(text)
	if len(sents) > maxEarSentences {
		return "more than 3 sentences"
	}
	if len(strings.Fields(text)) > maxEarWords {
		return "more than 50 words"
	}
	for _, sentence := range sents {
		if len(strings.Fields(sentence)) > maxSentenceWords {
			return "a sentence of more than 20 words"
		}
	}
	return ""
}

func sentenceTexts(text string) []string {
	var out []string
	start := 0
	for _, end := range earSentenceEnd.FindAllStringIndex(text, -1) {
		part := text[start:end[0]]
		if text[end[0]] == '.' && endsInAbbreviation(part) {
			continue
		}
		if strings.TrimSpace(part) != "" {
			out = append(out, part)
		}
		start = end[1]
	}
	if rest := text[start:]; strings.TrimSpace(rest) != "" {
		out = append(out, rest)
	}
	return out
}
func endsInAbbreviation(text string) bool {
	word := strings.TrimLeft(text[strings.LastIndexFunc(text, unicode.IsSpace)+1:], `("'`)
	return earAbbreviations[strings.ToLower(word)]
}

// EndsInAbbreviation reports whether the final full stop belongs to an R8
// abbreviation, not a sentence end.
func EndsInAbbreviation(text string) bool { return endsInAbbreviation(text) }
func clauses(text string) []string {
	var out []string
	for _, sentence := range sentenceTexts(text) {
		trimmed := strings.TrimSpace(sentence)
		if key := earKey.FindString(trimmed); key != "" && strings.HasPrefix(trimmed, key+": ") {
			out = append(out, key, trimmed[len(key)+2:])
			continue
		}
		out = append(out, sentence)
	}
	return out
}
func tokens(text string) []string {
	fields := strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && !strings.ContainsRune(".:%-'", r)
	})
	out := fields[:0]
	for _, field := range fields {
		field = strings.TrimSuffix(strings.Trim(field, ".:-'"), "'s")
		if field != "" {
			out = append(out, field)
		}
	}
	return out
}
func isName(token string) bool {
	first, _ := utf8.DecodeRuneInString(token)
	return unicode.IsUpper(first) && token != "I" && token != "PR"
}
func prNumbers(text string) []string {
	var out []string
	for _, match := range earPR.FindAllStringSubmatch(text, -1) {
		out = append(out, match[1])
	}
	return out
}
func set(items []string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, item := range items {
		m[item] = true
	}
	return m
}
