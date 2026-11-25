package concierge

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// spokenName is an agent session's title or name as the voice may say it:
// without emoji, their joiners and selectors, or control characters, on one
// line. A title such as "🍓 PI: Deploy the unit" is read aloud as
// "PI: Deploy the unit".
func spokenName(name string) string {
	kept := strings.Map(func(r rune) rune {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			return ' '
		case unicode.IsControl(r),
			unicode.In(r, unicode.So, unicode.Sk, unicode.Cf, unicode.Co, unicode.Cs, unicode.Me, unicode.Variation_Selector),
			r >= 0x1F3FB && r <= 0x1F3FF, // skin tone modifiers
			r == 0xFFFD:
			return -1
		default:
			return r
		}
	}, name)
	return strings.Join(strings.Fields(kept), " ")
}

// searchStopwords are the words of a spoken question that name no work:
// function words, question words, the wake word and talk about status.
var searchStopwords = map[string]bool{}

func init() {
	for _, word := range strings.Fields(`
		a an the and or but nor of to in on at for with about from by as into onto over under up down out off
		is are was were be been being am do does did done doing have has had having will would shall should can could may might must
		i me my mine we us our ours you your yours he him his she her hers it its they them their theirs this that these those there here
		what whats who whom whose which where when why how whether if then than so just also too very really quite any some all each every
		not no yes ok okay right well um uh oh hey hi hello please thanks thank tell say said ask asked know knew think thought see look find
		wispr whisper ghost agent agents issue issues ticket tickets thing things stuff work working worked works job task tasks
		happening happen happened happens going go goes gone get got status update updates updated latest new news progress
		change changes changed changing ready
		now today yesterday tomorrow yet still again ever last next first days day weeks week hours hour minutes minute time
		one two three lot lots much many more most less least other another same such own let lets like want need
		holds holding held holder owns owned owner owning
	`) {
		searchStopwords[word] = true
	}
}

// describedIssue finds where a turn describes the issue it means: "the issue
// about realtime spoken conversation", up to where the question goes on.
var describedIssue = regexp.MustCompile(`(?i)\b(?:issue|ticket|task|work|project|thing)s?\s+(?:about|on|for|called|named|of|with)\s+(.+?)(?:\s+(?:when|whether|if|and|but|what|how|why|who|which|so|because)\b|[?.!,;]|$)`)

// searchTerms is what Dispatch's keyword search is sent for words said in
// the room: each of their words that names work (namingWords), in order and
// once each, however many. A turn that describes the issue ("the issue about
// X") gives the description's words alone. What reads as an issue key
// ("infra 4592", but also "the last 30") is left out: a read tries it as a
// key. Empty when none of the words names work, and then nothing is
// searched. Live on 2026-11-24 these words found the issue meant for 5 of 10
// questions, as their first five did, and the whole question for 3 of 10.
func searchTerms(words string) string {
	if match := describedIssue.FindStringSubmatch(words); match != nil {
		if terms := strings.Join(namingWords(match[1]), " "); terms != "" {
			return terms
		}
	}
	return strings.Join(namingWords(words), " ")
}

// searchQuestion is what Dispatch's full search, words and meaning, is sent
// for words said in the room: the question as said after the wake phrase,
// whole, in lower case and without the punctuation around its words; a name
// spelled out is the one word it spells (spelledOut), "real time" is
// "realtime" (compound), and an issue key is left out, since a read tries it
// as a key: a key of a project the view holds, or any word that reads as a
// key while the view cannot tell. A question that still opens with the wake
// phrase, as the model may search one, is sent without it. Empty when none
// of the words names work (searchTerms). Live on 2026-11-24 the question put
// the issue meant first for 8 of 10 questions, against 4 of 10 for its first
// five words that name work and 5 of 10 for all of them; a key in it put the
// issue meant second.
func (v projectsView) searchQuestion(words string) string {
	terms := searchTerms(words)
	if terms == "" {
		return ""
	}
	text := possessiveKey.ReplaceAllStringFunc(spokenKeys(spelledOut(words)), func(key string) string {
		if !v.sure || v.holds(possessiveKey.FindStringSubmatch(key)[1]) {
			return " "
		}
		return key
	})
	text = compound.ReplaceAllString(text, "realtime")
	// A curly apostrophe is read as a straight one, as pick.go reads a
	// reply, so a question reads the same with either: a voice's model may
	// write the final turn, Deepgram the read-ahead's, and the answer reuses
	// the read-ahead only for the same question.
	var question []string
	for _, field := range strings.Fields(strings.ToLower(strings.ReplaceAll(text, "’", "'"))) {
		if word := strings.TrimFunc(field, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }); word != "" {
			question = append(question, word)
		}
	}
	if question = withoutWake(question); len(question) == 0 {
		// Its one word that names work was a spelling of the wake name
		// ("Wispa?"): Dispatch refuses an empty search with a 400.
		return terms
	}
	return strings.Join(question, " ")
}

// maxSearchLength is the most Dispatch searches: it refuses a query of more
// than 1000 characters with a 400 (`dispatch search --help`: "2 to 1000
// characters"), which the room would hear as Dispatch out of reach. A
// character there is a UTF-16 unit, and no character takes fewer bytes.
const maxSearchLength = 1000

// withinSearchLength is text cut to what Dispatch searches, after its last
// word that fits, or at the limit for a word longer than it.
func withinSearchLength(text string) string {
	if len(text) <= maxSearchLength {
		return text
	}
	if i := strings.LastIndexByte(text[:maxSearchLength+1], ' '); i > 0 {
		return text[:i]
	}
	cut := text[:maxSearchLength]
	for !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}

// possessiveKey is an issue key as spokenKey reads it, with the possessive
// said after it ("ABC-12's"), which goes with the key: left out alone, the
// key left a lone "s" in the question.
var possessiveKey = regexp.MustCompile(spokenKey.String() + `(?:['’]s\b)?`)

// wakeNames are how a transcript writes the name in the wake phrase, as the
// wake phrase's detector reads it (internal/wake/transcript), and
// wakeOpeners the words a call may start with before it.
var (
	wakeNames   = map[string]bool{"wispr": true, "whisper": true, "wisper": true, "whispr": true, "wispa": true, "whispa": true, "lispr": true}
	wakeOpeners = map[string]bool{"hey": true, "hi": true, "ok": true, "okay": true, "so": true, "um": true, "uh": true, "and": true}
)

// withoutWake is words, lower case and without punctuation, without the
// wake phrase they open with, after at most one opener ("hey wispr"):
// "wispr" or "ghost wispr", the name as Deepgram may spell it.
func withoutWake(words []string) []string {
	rest := words
	if len(rest) > 1 && wakeOpeners[rest[0]] {
		rest = rest[1:]
	}
	switch {
	case len(rest) > 1 && rest[0] == "ghost" && wakeNames[rest[1]]:
		return rest[2:]
	case len(rest) > 0 && wakeNames[rest[0]]:
		return rest[1:]
	}
	return words
}

// compound is a word Deepgram may write as two, or hyphened, where Dispatch's
// titles write one: "real time" and "real-time" are "realtime".
var compound = regexp.MustCompile(`(?i)\breal[- ]time\b`)

// spelledLetters are the words a name spelled out writes for a letter: the
// letter alone, except "a" and "i", which are words, and a letter's name
// that is no common word.
var spelledLetters = map[string]string{
	"ex": "x", "vee": "v", "bee": "b", "cee": "c", "dee": "d", "ef": "f", "eff": "f", "gee": "g", "aitch": "h",
	"jay": "j", "kay": "k", "el": "l", "ell": "l", "em": "m", "en": "n", "pee": "p", "cue": "q", "ess": "s",
	"tee": "t", "wye": "y", "zed": "z", "zee": "z",
}

func init() {
	for letter := 'b'; letter <= 'z'; letter++ {
		if letter != 'i' {
			spelledLetters[string(letter)] = string(letter)
		}
	}
}

// spelledDigits are the words a name spelled out writes for a digit.
var spelledDigits = map[string]string{
	"zero": "0", "one": "1", "two": "2", "three": "3", "four": "4", "five": "5", "six": "6", "seven": "7", "eight": "8", "nine": "9",
	"0": "0", "1": "1", "2": "2", "3": "3", "4": "4", "5": "5", "6": "6", "7": "7", "8": "8", "9": "9",
}

// spelledOut writes each name spelled out in text as the one word it spells
// ("the ex v six port" is "the xv6 port"), so the search gets the name, not
// letters too short to search or a digit's word. A name spelled out, as
// Deepgram writes one said letter by letter, is two or more whole words in a
// row, apart by spaces or hyphens, each a letter or a digit as spoken
// (spelledLetters, spelledDigits), at least one of them a letter; its last
// may be possessive ("ex v six's" is "xv6's"). A comma or a full stop ends
// it. Words of a run of digits alone ("one two") and part of a word ("what's
// one") stay as they were.
func spelledOut(text string) string {
	fields := strings.Fields(text)
	out := make([]string, 0, len(fields))
	var spelled strings.Builder
	var from []string // the words of the run under way, as written
	chars, letters := 0, false
	end := func(tail string) {
		if chars >= 2 && letters {
			out = append(out, spelled.String()+tail)
		} else {
			out = append(out, from...)
		}
		spelled.Reset()
		from, chars, letters = from[:0], 0, false
	}
	spelledWord := func(word string) bool {
		for _, part := range strings.Split(strings.ToLower(word), "-") {
			if spelledLetters[part] == "" && spelledDigits[part] == "" {
				return false
			}
		}
		return word != ""
	}
	for _, field := range fields {
		word := strings.TrimRight(field, ",.;:!?")
		tail := field[len(word):]
		if chars > 0 {
			for _, possessive := range []string{"'s", "’s"} {
				if name, ok := strings.CutSuffix(word, possessive); ok && spelledWord(name) {
					word, tail = name, possessive+tail
					break
				}
			}
		}
		if !spelledWord(word) {
			end("")
			out = append(out, field)
			continue
		}
		for _, part := range strings.Split(strings.ToLower(word), "-") {
			if letter := spelledLetters[part]; letter != "" {
				spelled.WriteString(letter)
				letters = true
			} else {
				spelled.WriteString(spelledDigits[part])
			}
			chars++
		}
		from = append(from, field)
		if tail != "" {
			end(tail)
		}
	}
	end("")
	return strings.Join(out, " ")
}

// sayKey is an issue key as people say one, for the voice: its project as a
// word when it reads as one ("Infra"), else letter by letter ("A B C",
// "A R S T"), and its number in pairs of digits ("forty-five ninety-two").
// The text-to-speech voice spells a key as written letter by letter and
// says each digit. Anything that is no key comes back as it was.
func sayKey(key string) string {
	project, number, ok := strings.Cut(key, "-")
	if !ok || project == "" || number == "" || strings.Trim(number, "0123456789") != "" {
		return key
	}
	return sayProject(project) + " " + sayNumber(number)
}

// sayProject is a project's name as the voice says it: as a word when it
// has three letters or more, a vowel, and at most one consonant after its
// last vowel ("Infra", "Topaz"), else letter by letter.
func sayProject(project string) string {
	lower := strings.ToLower(project)
	last := strings.LastIndexAny(lower, "aeiou")
	if len(lower) >= 3 && last >= 0 && len(lower)-last <= 2 && strings.IndexFunc(lower, func(r rune) bool { return r < 'a' || r > 'z' }) < 0 {
		return strings.ToUpper(lower[:1]) + lower[1:]
	}
	return strings.Join(strings.Split(strings.ToUpper(project), ""), " ")
}

var (
	unitWords = []string{"zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine"}
	teenWords = []string{"ten", "eleven", "twelve", "thirteen", "fourteen", "fifteen", "sixteen", "seventeen", "eighteen", "nineteen"}
	tensWords = []string{"", "", "twenty", "thirty", "forty", "fifty", "sixty", "seventy", "eighty", "ninety"}
)

// sayNumber is an issue number of digits as people say one: one or two
// digits as a number ("seven", "forty-one"), three as a digit and a pair
// ("three twenty-three"), four as two pairs ("fifteen ninety-seven", "ten
// oh five", "fifteen hundred", "two thousand"), and more digit by digit.
func sayNumber(digits string) string {
	switch len(digits) {
	case 1:
		return unitWords[digits[0]-'0']
	case 2:
		return sayPair(digits)
	case 3:
		return unitWords[digits[0]-'0'] + " " + sayLastPair(digits[1:])
	case 4:
		if digits[1:] == "000" {
			return unitWords[digits[0]-'0'] + " thousand"
		}
		return sayPair(digits[:2]) + " " + sayLastPair(digits[2:])
	}
	words := make([]string, 0, len(digits))
	for _, d := range digits {
		words = append(words, unitWords[d-'0'])
	}
	return strings.Join(words, " ")
}

// sayPair is two digits as a number: "forty-one", "twelve", "seven".
func sayPair(pair string) string {
	tens, units := pair[0]-'0', pair[1]-'0'
	switch {
	case tens == 0:
		return unitWords[units]
	case tens == 1:
		return teenWords[units]
	case units == 0:
		return tensWords[tens]
	}
	return tensWords[tens] + "-" + unitWords[units]
}

// sayLastPair is a number's last two digits after its first: "hundred" for
// 00, "oh five" for 05, else as a number.
func sayLastPair(pair string) string {
	switch {
	case pair == "00":
		return "hundred"
	case pair[0] == '0':
		return "oh " + unitWords[pair[1]-'0']
	}
	return sayPair(pair)
}

// namingWords is every word of text that names work, once each, in order: a
// spelled-out name as one word, an issue key left out, no stopword and no
// word under three letters.
func namingWords(text string) []string {
	var words []string
	seen := map[string]bool{}
	rest := compound.ReplaceAllString(spokenKey.ReplaceAllString(spokenKeys(spelledOut(text)), " "), "realtime")
	for _, word := range strings.FieldsFunc(strings.ToLower(rest), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '\''
	}) {
		word = strings.Trim(strings.TrimSuffix(word, "'s"), "'")
		if len([]rune(word)) < 3 || searchStopwords[word] || strings.IndexFunc(word, unicode.IsLetter) < 0 || seen[word] {
			continue
		}
		seen[word] = true
		words = append(words, word)
	}
	return words
}
