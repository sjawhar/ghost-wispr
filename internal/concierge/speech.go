package concierge

import (
	"regexp"
	"strings"
	"unicode"
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

// maxSearchTerms is how many words a search sends Dispatch.
const maxSearchTerms = 5

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

// searchTerms is what a search sends Dispatch for words said in the room:
// their words that name work, at most maxSearchTerms, in order and once each.
// A turn that describes the issue ("the issue about X") gives the
// description's words alone. What reads as an issue key ("infra 4592", but
// also "the last 30") is left out: a read tries it as a key. Dispatch gets
// the fewest words, never the sentence; empty when none of the words names
// work.
func searchTerms(words string) string {
	if match := describedIssue.FindStringSubmatch(words); match != nil {
		if terms := workWords(match[1]); terms != "" {
			return terms
		}
	}
	return workWords(words)
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
// (spelledLetters, spelledDigits), at least one of them a letter. A comma or
// a full stop ends it. Words of a run of digits alone ("one two") and part
// of a word ("what's one") stay as they were.
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
	for _, field := range fields {
		word := strings.TrimRight(field, ",.;:!?")
		parts := strings.Split(strings.ToLower(word), "-")
		ok := word != ""
		for _, part := range parts {
			ok = ok && (spelledLetters[part] != "" || spelledDigits[part] != "")
		}
		if !ok {
			end("")
			out = append(out, field)
			continue
		}
		for _, part := range parts {
			if letter := spelledLetters[part]; letter != "" {
				spelled.WriteString(letter)
				letters = true
			} else {
				spelled.WriteString(spelledDigits[part])
			}
			chars++
		}
		from = append(from, field)
		if tail := field[len(word):]; tail != "" {
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

// workWords is the words of text that name work, at most maxSearchTerms.
func workWords(text string) string {
	var terms []string
	seen := map[string]bool{}
	add := func(term string) {
		if len(terms) < maxSearchTerms && !seen[term] {
			seen[term] = true
			terms = append(terms, term)
		}
	}
	rest := compound.ReplaceAllString(spokenKey.ReplaceAllString(spokenKeys(spelledOut(text)), " "), "realtime")
	for _, word := range strings.FieldsFunc(strings.ToLower(rest), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '\''
	}) {
		word = strings.Trim(strings.TrimSuffix(word, "'s"), "'")
		if len([]rune(word)) < 3 || searchStopwords[word] || strings.IndexFunc(word, unicode.IsLetter) < 0 {
			continue
		}
		add(word)
	}
	return strings.Join(terms, " ")
}
