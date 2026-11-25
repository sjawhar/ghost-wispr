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
	"sync"
	"time"
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
	HandsUp                  // say which joined agents have a hand up
)

func (c Command) String() string {
	switch c {
	case Stop:
		return "stop"
	case GoAhead:
		return "go ahead"
	case NeverMind:
		return "never mind"
	case HandsUp:
		return "hands up"
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
// write it as configured, "Lispr" among them on 2026-11-21. They count only
// when the word ends in a comma or the text ends, or a question or request
// word follows, since "whisper" is also an ordinary word. "Mister" and the
// like, which Deepgram also wrote for the name, are a person's address and
// never count.
var soundsLike = map[string][]string{
	"wispr": {"whisper", "wisper", "whispr", "wispa", "whispa", "lispr"},
}

// commaOnly are the spellings of soundsLike that count only before a comma
// or at the end of the text, not before a call word: "Lispr, go ahead" is a
// call, but "Lispr is what the transcript printed" talks about the word.
var commaOnly = map[string]bool{"lispr": true}

// keytermsLike are the words a wake phrase is sent with as Deepgram
// keyterms, right after it, in room the phrases and keys leave (Keyterms):
// Nova-3 knows how "Whisper" sounds, and not the spelling "Wispr", which it
// heard as "Mister", "Miss Burr" and "Lispr".
var keytermsLike = map[string]string{"wispr": "Whisper"}

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
		exact, ok, strict := true, true, false
		for k, want := range p.words {
			got := normalise(tokens[at+k])
			if got == want {
				continue
			}
			if !soundsLikeWord(got, want) {
				ok = false
				break
			}
			exact, strict = false, strict || commaOnly[got]
		}
		if !ok {
			continue
		}
		last := at + len(p.words) - 1
		rest := tokens[last+1:]
		switch {
		case exact:
		case strict && !strings.HasSuffix(tokens[last], ",") && len(rest) > 0:
			continue
		case !callFollows(tokens[last], rest):
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
	case (first == "who" || first == "who's" || first == "whos") && saysHand(words), first == "any" && (second == "hand" || second == "hands"):
		return HandsUp
	}
	return Ask
}

// saysHand reports whether words hold "hand" or "hands": "who has a hand
// up?", not "who handles the budget?".
func saysHand(words []string) bool {
	return slices.ContainsFunc(words, func(w string) bool { n := normalise(w); return n == "hand" || n == "hands" })
}

// IssueKeys returns the issue keys text names, in the forms people say them:
// "INFRA-1027", "infra 1027", "infra ten twenty-seven", "infra one oh two
// seven", and "in for" before a number, as Deepgram writes "infra". A key is
// a word of letters and a number; the caller checks it against the keys it
// knows.
func IssueKeys(text string) []string { return issueKeys(text, false) }

// IssueKeysAfterName is IssueKeys for text said right after the wake
// phrase, as the turn of a call: "in" cannot begin a phrase there, so "in
// for" at its start is the project's name, before a date or a thousand too
// ("Wispr, in for 10/27?").
func IssueKeysAfterName(text string) []string { return issueKeys(text, true) }

func issueKeys(text string, afterName bool) []string {
	var keys []string
	for _, span := range keySpans(keyTokens(text), afterName) {
		keys = append(keys, span.key)
	}
	return keys
}

// WriteKeys writes each key of projects that text says in a spoken form as
// the key ("in for forty five ninety two" as INFRA-4592), so the Concierge
// and Dispatch read it; other words stay as they were. "One" after a
// project is as often a pronoun as a number ("the INFRA one", "is INFRA one
// of them"), so a key of one digit said so is not written.
func WriteKeys(text string, projects []string) string { return writeKeys(text, projects, false) }

// WriteKeysAfterName is WriteKeys for text said right after the wake phrase
// (IssueKeysAfterName).
func WriteKeysAfterName(text string, projects []string) string {
	return writeKeys(text, projects, true)
}

func writeKeys(text string, projects []string, afterName bool) string {
	known := make(map[string]bool, len(projects))
	for _, p := range projects {
		known[strings.ToUpper(p)] = true
	}
	tokens := keyTokens(text)
	var out []string
	next := 0
	for _, span := range keySpans(tokens, afterName) {
		project, _, _ := strings.Cut(span.key, "-")
		if !known[project] {
			continue
		}
		if _, number, _ := strings.Cut(span.key, "-"); len(number) == 1 && normalise(tokens[span.to-1].text) == "one" {
			continue
		}
		out = append(out, joinTokens(tokens[next:span.from])...)
		// The key keeps the punctuation its last word had ("…ninety two?").
		last := tokens[span.to-1].text
		out = append(out, span.key+last[len(strings.TrimRightFunc(last, notWordRune)):])
		next = span.to
	}
	if next == 0 {
		return text
	}
	out = append(out, joinTokens(tokens[next:])...)
	return strings.Join(out, " ")
}

func notWordRune(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }

// token is a word of text; hyphen marks one that was split from the word
// before it at a hyphen ("twenty-seven").
type token struct {
	text   string
	hyphen bool
}

// keyTokens splits text into words, and "twenty-seven" into two number
// words; "INFRA-1027" stays one word.
func keyTokens(text string) []token {
	var tokens []token
	for _, t := range strings.Fields(text) {
		if strings.Contains(t, "-") && !keyToken(t) {
			for i, part := range strings.Split(t, "-") {
				tokens = append(tokens, token{text: part, hyphen: i > 0})
			}
			continue
		}
		tokens = append(tokens, token{text: t})
	}
	return tokens
}

// joinTokens gives tokens back as words, rejoining those split at a hyphen.
func joinTokens(tokens []token) []string {
	var words []string
	for _, t := range tokens {
		if t.hyphen && len(words) > 0 {
			words[len(words)-1] += "-" + t.text
			continue
		}
		words = append(words, t.text)
	}
	return words
}

// keySpan is a key text says, over tokens [from, to).
type keySpan struct {
	key      string
	from, to int
}

// soundsLikeProject lists the two words Deepgram writes for a project word
// it does not know; they read as the project only before a number of three or
// more digits.
var soundsLikeProject = map[[2]string]string{{"in", "for"}: "INFRA"}

// beforeAName lists the words after which "in" cannot begin a phrase, so
// "in for" there is a project's name heard as two words: "the status of in
// for 10/27".
var beforeAName = map[string]bool{"of": true, "about": true, "on": true, "with": true, "to": true, "for": true}

// keySpans finds the keys tokens say. With afterName, the tokens follow the
// wake phrase, so their start is where a name goes too (beforeAName).
func keySpans(tokens []token, afterName bool) []keySpan {
	texts := make([]string, len(tokens))
	for i, t := range tokens {
		texts[i] = t.text
	}
	var spans []keySpan
	for i := 0; i < len(tokens); i++ {
		word := strings.TrimSuffix(normalise(texts[i]), "'s")
		if keyToken(texts[i]) {
			prefix, digits, _ := strings.Cut(word, "-")
			spans = append(spans, keySpan{key: strings.ToUpper(prefix) + "-" + digits, from: i, to: i + 1})
			continue
		}
		if i+1 < len(tokens) {
			if project, ok := soundsLikeProject[[2]string{word, normalise(texts[i+1])}]; ok {
				// Only before a number of three or more digits, as issue
				// numbers are, and no year: "in for thirty minutes" and "in
				// for 2027" are no keys. A date smart formatting wrote, or a
				// number said with a thousand, forms main did not read as a
				// number, reads as the project only where a name goes: after
				// a word "in" cannot follow, or right after the wake phrase.
				// "Pencilled in for 10/27" is a date, and "in for five
				// thousand dollars" an amount. A number said with a hundred
				// reads as main read it: "go ahead, in for forty five hundred
				// and two" is INFRA-4502.
				digits, used := spokenNumber(texts[i+2:])
				named := i == 0 && afterName || i > 0 && beforeAName[normalise(texts[i-1])]
				if len(digits) >= 3 && !aYear(digits) && (!amountLike(texts[i+2:i+2+used]) || named) {
					spans = append(spans, keySpan{key: project + "-" + digits, from: i, to: i + 2 + used})
					i += 1 + used
					continue
				}
			}
		}
		if !lettersOnly(word) || len(word) < 2 || numberWord(word) {
			continue
		}
		digits, used := spokenNumber(texts[i+1:])
		if digits == "" {
			continue
		}
		spans = append(spans, keySpan{key: strings.ToUpper(word) + "-" + digits, from: i, to: i + 1 + used})
		i += used
	}
	return spans
}

// amountLike reports whether number, the tokens of one spoken or written
// number, is in a form main did not read as a number and that reads as a
// date or an amount more readily than as an issue number: a date smart
// formatting wrote ("10/27"), or a number said with a thousand ("five
// thousand").
func amountLike(number []string) bool {
	for _, w := range number {
		if strings.Contains(w, "/") || normalise(w) == "thousand" {
			return true
		}
	}
	return false
}

// aYear reports whether digits read as a year of this era (1900 to 2099),
// which "in for" before it is not a project: "we're in for 2027".
func aYear(digits string) bool {
	return len(digits) == 4 && (strings.HasPrefix(digits, "19") || strings.HasPrefix(digits, "20"))
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
	return o || te || tn || w == "hundred" || w == "thousand"
}

// writtenGroups joins the groups of a number smart formatting wrote split.
var writtenGroups = strings.NewReplacer(",", "", "/", "")

// spokenNumber reads the digits that tokens open with: written digits, or
// number words read in groups ("ten twenty-seven" is 10 then 27, "one oh two
// seven" is 1, 0, 2, 7, "eleven hundred five" is 1105, "one thousand and
// twenty seven" is 1027). It reports the digits and how many tokens it read.
func spokenNumber(tokens []string) (string, int) {
	if len(tokens) > 0 {
		// Written digits, with the grouping commas smart formatting writes
		// ("4,592"), or the slash of the date it writes for a number said
		// in two groups ("10/27").
		if w := writtenGroups.Replace(strings.TrimSuffix(normalise(tokens[0]), "'s")); digitsOnly(w) {
			return w, 1
		}
	}
	var b strings.Builder
	used := 0
	for used < len(tokens) {
		w := normalise(tokens[used])
		if places := scalePlaces[w]; places > 0 && b.Len() > 0 {
			// "eleven hundred five": what follows fills the hundred's two
			// places, a thousand's three; "forty five hundred and two" says
			// "and" first.
			after := tokens[used+1:]
			skip := 0
			if len(after) > 1 && normalise(after[0]) == "and" {
				after, skip = after[1:], 1
			}
			rest, n := spokenNumber(after)
			if len(rest) > places || n == 0 {
				rest, n, skip = "", 0, 0
			}
			b.WriteString(strings.Repeat("0", places-len(rest)) + rest)
			return b.String(), used + 1 + skip + n
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

// scalePlaces is how many digits follow a scale word of a spoken number.
var scalePlaces = map[string]int{"hundred": 2, "thousand": 3}

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
// phrases first, then issue keys, each once, within Deepgram's limits, as
// before; each wake phrase's keyterm-like word (keytermsLike) goes right
// after it when the list has room left, so it never displaces one of them.
func Keyterms(phrases, issueKeys []string) []string {
	out := make([]string, 0, len(phrases)+len(issueKeys)+len(keytermsLike))
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
	// Each wake phrase's keyterm-like word goes right after it, in the room
	// the list has left: it never displaces a phrase or a key.
	for i := 0; i < len(out); i++ {
		like, ok := keytermsLike[normalise(out[i])]
		if !ok || seen[like] || len(out) == MaxKeyterms || tokens+len(strings.Fields(like)) > MaxKeytermTokens {
			continue
		}
		seen[like] = true
		tokens += len(strings.Fields(like))
		out = slices.Insert(out, i+1, like)
		i++
	}
	return out
}

// Configurer is a live Deepgram socket as LiveKeyterms reaches it: Configure
// asks it to replace its keyterms mid-stream and returns at once, and the
// hook OnConfigured gives hears Deepgram's answer to each Configure, with the
// keyterms it answers: taken or refused. A socket sends one Configure at a
// time, as Deepgram takes them (CONFIGURE_ALREADY_PENDING), and one that
// closes first never answers.
type Configurer interface {
	Configure(keyterms []string) error
	OnConfigured(answer func(keyterms []string, taken bool))
}

// configureRetryAfter is how long a list Deepgram refused waits before it
// goes once more.
const configureRetryAfter = time.Second

// LiveKeyterms are the keyterms of the live Deepgram stream (R4): the wake
// phrases, then the issue keys of the agents that joined the meeting, then
// Dispatch's project keys, within Deepgram's limits (Keyterms). A dial reads
// List. A join or a leave that changes the list sends it to the socket that
// carries the meeting with Configure, so the stream hears an agent's key from
// its join on, with no redial. The list is the stream's only once Deepgram
// takes it: a refused list stays wanted.
type LiveKeyterms struct {
	phrases    []string
	projects   func() []string
	retryAfter time.Duration

	mu      sync.Mutex
	joined  []string
	stream  Configurer
	taken   []string    // the list stream has: its dial's, or the last Deepgram took
	asking  bool        // a Configure to stream waits for Deepgram's answer
	retry   *time.Timer // the resend of a refused list, while it waits
	retried bool        // a list refused since the last change had its resend
	refused bool        // Deepgram refused a list, and stream lacks the one wanted since
	refusal func(refused bool)
}

// NewLiveKeyterms returns the live stream's keyterms for the wake phrases;
// projects, nil for none, gives Dispatch's project keys at each read.
func NewLiveKeyterms(phrases []string, projects func() []string) *LiveKeyterms {
	return &LiveKeyterms{phrases: slices.Clone(phrases), projects: projects, retryAfter: configureRetryAfter}
}

// SetRefusalHook makes hook hear, each time it changes, whether the stream
// lacks the wanted list because Deepgram refused one: true at a refusal that
// leaves the stream without the wanted list, false once the stream has it
// (Deepgram took it, or a join or a leave made it the list the stream has),
// once Deepgram takes another list, or once a new dial attaches.
func (k *LiveKeyterms) SetRefusalHook(hook func(refused bool)) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.refusal = hook
}

// List is the keyterms a dial sends now.
func (k *LiveKeyterms) List() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.listLocked()
}

func (k *LiveKeyterms) listLocked() []string {
	keys := slices.Clone(k.joined)
	if k.projects != nil {
		keys = append(keys, k.projects()...)
	}
	return Keyterms(k.phrases, keys)
}

// SetJoined makes keys the joined agents' issue keys, and sends the socket
// the new list when it changed.
func (k *LiveKeyterms) SetJoined(keys []string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.joined = slices.Clone(keys)
	k.retried = false
	k.sendLocked()
}

// Attach makes stream, a socket dialled with the list dialed, the one that
// carries the meeting; it is sent the list at once when a join or a leave
// changed it since the dial. nil detaches the last one. The answer of a
// socket attached before is not heard.
func (k *LiveKeyterms) Attach(stream Configurer, dialed []string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.stopRetryLocked()
	k.stream, k.taken, k.asking, k.retried = stream, slices.Clone(dialed), false, false
	if stream != nil {
		stream.OnConfigured(func(list []string, taken bool) { k.answered(stream, list, taken) })
	}
	k.reportLocked(false)
	k.sendLocked()
}

// sendLocked sends the stream the list when it does not have it, unless a
// Configure waits for its answer, which sends the newest list then. A stream
// that has the list no longer lacks it after a refusal, whether or not a
// Configure waits. A send that fails is not retried: the socket is closing,
// and the next dial reads List.
func (k *LiveKeyterms) sendLocked() {
	if k.stream == nil {
		return
	}
	list := k.listLocked()
	if slices.Equal(list, k.taken) {
		k.reportLocked(false)
		return
	}
	if k.asking {
		return
	}
	k.stopRetryLocked()
	if k.stream.Configure(list) == nil {
		k.asking = true
	}
}

// answered takes Deepgram's answer to the Configure of list on stream. Taken,
// list is the stream's. Refused, the stream keeps the list it had: a newer
// list goes at once; the same list goes once more after retryAfter, and a
// second refusal of it waits for the next join or leave, or the next dial. A
// refusal leaves the stream lacking the wanted list unless a join or a leave
// since made it the list the stream has.
func (k *LiveKeyterms) answered(stream Configurer, list []string, taken bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if stream != k.stream {
		return
	}
	k.asking = false
	if taken {
		k.taken = list
	}
	wanted := k.listLocked()
	k.reportLocked(!taken && !slices.Equal(wanted, k.taken))
	if !taken && slices.Equal(wanted, list) {
		if !k.retried {
			k.retried = true
			var retry *time.Timer
			retry = time.AfterFunc(k.retryAfter, func() {
				k.mu.Lock()
				defer k.mu.Unlock()
				if k.retry == retry {
					k.retry = nil
					k.sendLocked()
				}
			})
			k.retry = retry
		}
		return
	}
	k.retried = false
	k.sendLocked()
}

// stopRetryLocked drops the resend that waits, if any: a send or a new dial
// takes its place.
func (k *LiveKeyterms) stopRetryLocked() {
	if k.retry != nil {
		k.retry.Stop()
		k.retry = nil
	}
}

// reportLocked makes the stream lack the wanted list after a refusal, or not,
// and tells the refusal hook when that changes.
func (k *LiveKeyterms) reportLocked(refused bool) {
	if refused == k.refused {
		return
	}
	k.refused = refused
	if k.refusal != nil {
		k.refusal(refused)
	}
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
