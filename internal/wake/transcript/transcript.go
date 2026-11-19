// Package transcript finds Wispr's wake phrase in the live transcript.
//
// People address Wispr plainly ("Wispr, what is happening with …?"), so the
// rule is recognition of the phrase at the start of a sentence, never
// guessing whom a sentence addresses: "I will whisper it to you" is not a
// call.
package transcript

import (
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// Deepgram's limits on keyterms for one live request.
const (
	MaxKeyterms      = 100
	MaxKeytermTokens = 500
)

// Command is what the words after the wake phrase ask for.
type Command int

const (
	Ask       Command = iota // a question for the Concierge
	Stop                     // stop speaking and end the exchange
	GoAhead                  // speak the pending answer
	NeverMind                // cancel the newest question to an agent
)

func (c Command) String() string {
	switch c {
	case Stop:
		return "stop"
	case GoAhead:
		return "go ahead"
	case NeverMind:
		return "never mind"
	default:
		return "ask"
	}
}

// thanksWords open a thank-you.
var thanksWords = map[string]bool{"thanks": true, "thank": true, "cheers": true}

// Thanks reports whether text is a thank-you to Wispr ("Thanks, Wispr."),
// which ends an exchange: a thanks word, an optional "you", then the name,
// and nothing after it.
func (d *Detector) Thanks(text string) bool {
	tokens := strings.Fields(text)
	if len(tokens) < 2 || !thanksWords[normalise(tokens[0])] {
		return false
	}
	at := 1
	if normalise(tokens[1]) == "you" {
		at = 2
	}
	for _, p := range d.phrases {
		if at+len(p.words) != len(tokens) {
			continue
		}
		matched := true
		for k, want := range p.words {
			got := normalise(tokens[at+k])
			if got != want && !soundsLikeWord(got, want) {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

// Match is a wake phrase heard at the start of a sentence.
type Match struct {
	Phrase string // the configured phrase, as written in the configuration
	Rest   string // the words after it, as Deepgram wrote them
}

// Detector finds the wake phrase in transcript text.
type Detector struct {
	phrases []phrase // longest first, so "Ghost Wispr" wins over "Wispr"
}

type phrase struct {
	text  string
	words []string // normalised
}

// openers are words a call may start with before the phrase.
var openers = map[string]bool{"hey": true, "hi": true, "ok": true, "okay": true, "so": true, "um": true, "uh": true, "and": true}

// soundsLike lists the spellings Deepgram gives the name when it does not
// write it as configured. They count only when the word ends in a comma or
// the text ends, or a question or request word follows, since "whisper" is
// also an ordinary word.
var soundsLike = map[string][]string{
	"wispr": {"whisper", "wisper", "whispr", "wispa", "whispa"},
}

// callWords may follow a sound-alike spelling of the name in a call.
var callWords = map[string]bool{
	"what": true, "what's": true, "whats": true, "who": true, "who's": true, "where": true, "when": true,
	"why": true, "how": true, "is": true, "are": true, "can": true, "could": true, "would": true, "will": true,
	"tell": true, "go": true, "stop": true, "please": true, "do": true, "does": true, "did": true,
	"any": true, "give": true, "read": true, "ask": true, "find": true, "be": true, "never": true,
}

// New returns a Detector for phrases, such as "Wispr" and "Ghost Wispr".
func New(phrases []string) *Detector {
	d := &Detector{}
	for _, p := range phrases {
		words := strings.Fields(p)
		norm := make([]string, 0, len(words))
		for _, w := range words {
			if n := normalise(w); n != "" {
				norm = append(norm, n)
			}
		}
		if len(norm) > 0 {
			d.phrases = append(d.phrases, phrase{text: strings.TrimSpace(p), words: norm})
		}
	}
	// Longest first; stable for equal lengths, so configuration order wins.
	slices.SortStableFunc(d.phrases, func(a, b phrase) int { return len(b.words) - len(a.words) })
	return d
}

// Find returns the first wake phrase that opens a sentence of text, after at
// most one opener such as "hey".
func (d *Detector) Find(text string) (Match, bool) {
	tokens := strings.Fields(text)
	sentenceStart := true
	for i := range tokens {
		start := sentenceStart
		sentenceStart = endsSentence(tokens[i])
		if !start {
			continue
		}
		at := i
		if openers[normalise(tokens[i])] && !endsSentence(tokens[i]) && i+1 < len(tokens) {
			at = i + 1
		}
		if m, ok := d.matchAt(tokens, at); ok {
			return m, true
		}
	}
	return Match{}, false
}

// matchAt matches a phrase starting at tokens[at].
func (d *Detector) matchAt(tokens []string, at int) (Match, bool) {
	for _, p := range d.phrases {
		if at+len(p.words) > len(tokens) {
			continue
		}
		exact, ok := true, true
		for k, want := range p.words {
			got := normalise(tokens[at+k])
			if got == want {
				continue
			}
			if !soundsLikeWord(got, want) {
				ok = false
				break
			}
			exact = false
		}
		if !ok {
			continue
		}
		last := at + len(p.words) - 1
		rest := tokens[last+1:]
		if !exact && !callFollows(tokens[last], rest) {
			continue
		}
		return Match{Phrase: p.text, Rest: strings.Join(rest, " ")}, true
	}
	return Match{}, false
}

// IsName reports whether word is how Deepgram writes the first word of a wake
// phrase: as configured, or as a sound-alike spelling.
func (d *Detector) IsName(word string) bool {
	n := normalise(word)
	if n == "" {
		return false
	}
	for _, p := range d.phrases {
		if n == p.words[0] || soundsLikeWord(n, p.words[0]) {
			return true
		}
	}
	return false
}

func soundsLikeWord(got, want string) bool {
	for _, s := range soundsLike[want] {
		if got == s {
			return true
		}
	}
	return false
}

// callFollows reports whether a sound-alike spelling ending in last reads as
// a call: a comma after it, nothing after it, or a call word next.
func callFollows(last string, rest []string) bool {
	if strings.HasSuffix(last, ",") || len(rest) == 0 {
		return true
	}
	return callWords[normalise(rest[0])]
}

// CommandOf reads what the words after the wake phrase ask for.
func CommandOf(rest string) Command {
	words := strings.Fields(rest)
	norm := make([]string, 0, 3)
	for _, w := range words {
		if n := normalise(w); n != "" {
			norm = append(norm, n)
		}
		if len(norm) == 3 {
			break
		}
	}
	if len(norm) == 0 {
		return Ask
	}
	first := norm[0]
	second := ""
	if len(norm) > 1 {
		second = norm[1]
	}
	switch {
	case first == "stop" || first == "quiet" || first == "enough" || first == "cancel":
		return Stop
	case first == "be" && second == "quiet", first == "shut" && second == "up", first == "that's" && second == "enough", first == "thats" && second == "enough":
		return Stop
	case first == "go" && (second == "ahead" || second == "on"):
		return GoAhead
	case first == "nevermind", first == "never" && second == "mind", first == "forget" && second == "it":
		return NeverMind
	}
	return Ask
}

// IssueKeys returns the issue keys text names, in the forms people say them:
// "INFRA-1027", "infra 1027", "infra ten twenty-seven", "infra one oh two
// seven". A key is a word of letters and a number; the caller checks it
// against the keys it knows.
func IssueKeys(text string) []string {
	var tokens []string
	for _, t := range strings.Fields(text) {
		// "twenty-seven" is two number words; "INFRA-1027" stays one token.
		if strings.Contains(t, "-") && !keyToken(t) {
			tokens = append(tokens, strings.Split(t, "-")...)
			continue
		}
		tokens = append(tokens, t)
	}
	var keys []string
	for i := 0; i < len(tokens); i++ {
		word := strings.TrimSuffix(normalise(tokens[i]), "'s")
		if keyToken(tokens[i]) {
			prefix, digits, _ := strings.Cut(word, "-")
			keys = append(keys, strings.ToUpper(prefix)+"-"+digits)
			continue
		}
		if !lettersOnly(word) || len(word) < 2 || numberWord(word) {
			continue
		}
		digits, used := spokenNumber(tokens[i+1:])
		if digits == "" {
			continue
		}
		keys = append(keys, strings.ToUpper(word)+"-"+digits)
		i += used
	}
	return keys
}

// keyToken reports a written key such as "INFRA-1027", with punctuation
// around it.
func keyToken(t string) bool {
	prefix, digits, ok := strings.Cut(strings.TrimSuffix(normalise(t), "'s"), "-")
	return ok && len(prefix) >= 2 && lettersOnly(prefix) && digitsOnly(digits)
}

var (
	ones  = map[string]int{"zero": 0, "oh": 0, "one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6, "seven": 7, "eight": 8, "nine": 9}
	teens = map[string]int{"ten": 10, "eleven": 11, "twelve": 12, "thirteen": 13, "fourteen": 14, "fifteen": 15, "sixteen": 16, "seventeen": 17, "eighteen": 18, "nineteen": 19}
	tens  = map[string]int{"twenty": 20, "thirty": 30, "forty": 40, "fifty": 50, "sixty": 60, "seventy": 70, "eighty": 80, "ninety": 90}
)

func numberWord(w string) bool {
	_, o := ones[w]
	_, te := teens[w]
	_, tn := tens[w]
	return o || te || tn || w == "hundred"
}

// spokenNumber reads the digits that tokens open with: written digits, or
// number words read in groups ("ten twenty-seven" is 10 then 27, "one oh two
// seven" is 1, 0, 2, 7, "eleven hundred five" is 1105). It reports the digits
// and how many tokens it read.
func spokenNumber(tokens []string) (string, int) {
	if len(tokens) > 0 {
		if w := strings.TrimSuffix(normalise(tokens[0]), "'s"); digitsOnly(w) {
			return w, 1
		}
	}
	var b strings.Builder
	used := 0
	for used < len(tokens) {
		w := normalise(tokens[used])
		if w == "hundred" && b.Len() > 0 {
			// "eleven hundred five": what follows fills the hundred's two places.
			rest, n := spokenNumber(tokens[used+1:])
			if len(rest) > 2 {
				rest, n = "", 0
			}
			b.WriteString(strings.Repeat("0", 2-len(rest)) + rest)
			return b.String(), used + 1 + n
		}
		o, isOne := ones[w]
		te, isTeen := teens[w]
		tn, isTens := tens[w]
		switch {
		case isOne:
			b.WriteString(strconv.Itoa(o))
		case isTeen:
			b.WriteString(strconv.Itoa(te))
		case isTens:
			if used+1 < len(tokens) {
				if next, ok := ones[normalise(tokens[used+1])]; ok && next > 0 {
					tn += next
					used++
				}
			}
			b.WriteString(strconv.Itoa(tn))
		default:
			return b.String(), used
		}
		used++
	}
	return b.String(), used
}

func lettersOnly(w string) bool {
	for _, r := range w {
		if r < 'a' || r > 'z' {
			return false
		}
	}
	return w != ""
}

func digitsOnly(w string) bool {
	for _, r := range w {
		if r < '0' || r > '9' {
			return false
		}
	}
	return w != ""
}

// Keyterms returns the keyterms for Deepgram's live options: the wake
// phrases first, then issue keys, each once, within Deepgram's limits.
func Keyterms(phrases, issueKeys []string) []string {
	out := make([]string, 0, len(phrases)+len(issueKeys))
	seen := make(map[string]bool, cap(out))
	tokens := 0
	for _, list := range [][]string{phrases, issueKeys} {
		for _, k := range list {
			k = strings.Join(strings.Fields(k), " ")
			n := len(strings.Fields(k))
			if k == "" || seen[k] || len(out) == MaxKeyterms || tokens+n > MaxKeytermTokens {
				continue
			}
			seen[k] = true
			tokens += n
			out = append(out, k)
		}
	}
	return out
}

// normalise lowercases w and strips the punctuation around it, keeping an
// inner apostrophe ("what's").
func normalise(w string) string {
	return strings.ToLower(strings.TrimFunc(w, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}))
}

func endsSentence(w string) bool {
	w = strings.TrimRight(w, `"')`)
	return strings.HasSuffix(w, ".") || strings.HasSuffix(w, "?") || strings.HasSuffix(w, "!")
}
