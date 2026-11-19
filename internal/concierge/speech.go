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
	rest := compound.ReplaceAllString(spokenKey.ReplaceAllString(text, " "), "realtime")
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
