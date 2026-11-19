// Package transcript finds Wispr's wake phrase in the live transcript.
//
// People address Wispr plainly ("Wispr, what is happening with …?"), so the
// rule is recognition of the phrase at the start of a sentence, never
// guessing whom a sentence addresses: "I will whisper it to you" is not a
// call.
package transcript

import (
	"slices"
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
	Ask     Command = iota // a question for the Concierge
	Stop                   // stop speaking and end the exchange
	GoAhead                // speak the pending answer
)

func (c Command) String() string {
	switch c {
	case Stop:
		return "stop"
	case GoAhead:
		return "go ahead"
	default:
		return "ask"
	}
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
	}
	return Ask
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
