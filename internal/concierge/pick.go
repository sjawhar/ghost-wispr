package concierge

import (
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// offeredPick is what an answer with no one clear match offered (R5): the
// question asked, and the two issues it named, best first.
type offeredPick struct {
	at         time.Time
	sessionID  string
	turn       string
	candidates []candidate
}

// pickWords say which of the two offered issues a person picks. "One" is
// none: "the one about the wake word" names the issue by its title.
var pickWords = map[string]int{
	"first": 0, "former": 0, "1st": 0,
	"second": 1, "latter": 1, "other": 1, "2nd": 1,
}

// wakeKey names an exchange by its wake alone: its session id may come only
// after the wake.
func wakeKey(x *voice.Exchange) string {
	return x.WakeAt.UTC().Format(time.RFC3339Nano)
}

// offerPick keeps, for x's next turn, the question turn and the issues the
// voice offered.
func (c *Concierge) offerPick(x *voice.Exchange, turn string, candidates []candidate) {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, p := range c.picks {
		if now.Sub(p.at) > namedFor {
			delete(c.picks, key)
		}
	}
	c.picks[wakeKey(x)] = offeredPick{at: now, sessionID: x.SessionID, turn: turn, candidates: candidates}
}

// takePick reads turn as a reply to the issues x's last answer offered. A
// whole no ("no", "nope, not that one") picks none. Any other reply that is
// no confirmation (confirms: a question mark, a question word or a negation)
// is a new question, whatever key, place or title it names. A confirmation
// picks an issue only when it is a selection of one (pickIn): talk that holds
// a cue ("first coffee, then lunch") picks none. It returns the question
// first asked and the picked key. Either way the offer is used up: a turn
// that picks nothing is a question of its own.
func (c *Concierge) takePick(x *voice.Exchange, turn string) (asked, key string, ok bool) {
	c.mu.Lock()
	p, offered := c.picks[wakeKey(x)]
	delete(c.picks, wakeKey(x))
	c.mu.Unlock()
	if !offered {
		return "", "", false
	}
	// An offered issue is real, whatever the project list says: its key,
	// said letter by letter ("A B C seven") or of one digit ("INFRA-7"),
	// is a key of the reply. A no is a no whatever word and number of one
	// digit it names: "no, not INFRA-7".
	view := offeredView(p.candidates)
	turn, _ = view.readSpelledKeys(turn)
	reply := view.withoutOneDigitKeys(turn)
	switch {
	case declines(withoutOneDigitWords(reply)):
		return p.turn, "", true
	case !confirms(reply):
		return "", "", false
	}
	if picked := pickIn(p.candidates, turn); picked != "" {
		return p.turn, picked, true
	}
	return "", "", false
}

// dropPick uses up the offer of x's last answer with no pick: a turn the
// Concierge cannot read is a question of its own.
func (c *Concierge) dropPick(x *voice.Exchange) {
	c.mu.Lock()
	delete(c.picks, wakeKey(x))
	c.mu.Unlock()
}

// offeredView is a view of the projects of the offered issues, the keys of
// which are Dispatch's: a key of one digit of one of them reads as a key.
func offeredView(candidates []candidate) projectsView {
	view := projectsView{known: map[string]bool{}, sure: true}
	for _, candidate := range candidates {
		project, _, _ := strings.Cut(candidate.key, "-")
		view.known[strings.ToUpper(project)] = true
	}
	return view
}

// withoutOneDigitWords is turn with each word before a number of one digit
// taken out, whatever the word: "no, not INFRA-7" is "no, not".
func withoutOneDigitWords(turn string) string {
	return anyDigitsKey.ReplaceAllStringFunc(turn, func(match string) string {
		if parts := anyDigitsKey.FindStringSubmatch(match); len(keyGroups.Replace(parts[2])) == 1 && parts[3] == "" {
			return " "
		}
		return match
	})
}

var (
	// replyFillers may begin a reply: "uh, yes".
	replyFillers = set(strings.Fields("uh um oh well hmm mm"))
	// negations make a reply no confirmation, as does any word that ends
	// in "n't".
	negations = set(strings.Fields("no not nope nah never neither nor none cannot"))
	// yesWords may begin a yes, and yesTails follow one: "yes", "yeah,
	// that one", "sure, that's it".
	yesWords = set(strings.Fields("yes yeah yea yep yup sure correct right exactly ok okay"))
	yesTails = set([]string{"", "please", "that one", "this one", "that's it", "that's the one", "that's right", "the first one", "thanks", "thank you"})
	// yesPhrases are a yes alone, maybe with "please".
	yesPhrases = set([]string{"that one", "this one", "that's it", "that's the one", "that's right", "the first one", "that one please", "the first one please"})
	// noWords may begin a no, and noTails follow one: "nope, not that one";
	// noPhrases are a no alone.
	noWords   = set(strings.Fields("no nope nah"))
	noTails   = set([]string{"", "not", "not that", "not that one", "not this one", "thanks", "thank you", "neither"})
	noPhrases = set([]string{"not that one", "not this one", "neither", "neither one"})
	// selectionWords may stand beside the cues of a selection and add
	// nothing to it: a filler, a yes, and the few words people put around a
	// choice ("the one", "that's the wake word issue", "please use the first
	// one", "I think the first one", "probably the second one", "the first
	// one then"). Any other word is substance: the reply is talk, not a
	// selection. The list is kept narrow, and a phrase outside it is a new
	// question by design ("let's do the first one", "go for the first one",
	// "I'd say the wake word one"): a missed pick costs a new answer, where a
	// wrong one sends a question nobody asked.
	selectionWords = set(strings.Fields(`uh um oh hmm mm okay ok so well yeah yea yes yep yup sure right correct exactly please thanks thank you
		the a one that that's this it it's issue ticket i i'll i'd mean meant use take pick choose go with want about
		think guess then maybe probably actually just now`))
)

// replyWords are turn's words, lower case, its issue keys and the fillers it
// begins with left out; an apostrophe stays in its word ("that's").
func replyWords(turn string) []string {
	rest := compound.ReplaceAllString(spokenKey.ReplaceAllString(projectsView{}.spokenKeys(turn), " "), "realtime")
	words := strings.FieldsFunc(strings.ToLower(strings.ReplaceAll(rest, "’", "'")), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '\''
	})
	for len(words) > 0 && replyFillers[words[0]] {
		words = words[1:]
	}
	return words
}

// declines reports whether turn is a whole no: "no", "nope, not that one",
// "no, not ABC-12", "neither".
func declines(turn string) bool {
	words := replyWords(turn)
	if strings.Contains(turn, "?") || len(words) == 0 {
		return false
	}
	return noWords[words[0]] && noTails[strings.Join(words[1:], " ")] || noPhrases[strings.Join(words, " ")]
}

// confirms reports whether turn may confirm an offered issue: it holds no
// question mark, no question word or verb a question puts first
// (questionWords, auxiliaries, "am"; "what's" is "what") and no negation
// (negations, or a word that ends in "n't"), anywhere: "the first one, do I
// pick that" asks. Every way a reply picks an issue (pickIn) runs only behind
// it.
func confirms(turn string) bool {
	if strings.Contains(turn, "?") {
		return false
	}
	for _, word := range replyWords(turn) {
		base, _, _ := strings.Cut(word, "'")
		if questionWords[base] || auxiliaries[base] || base == "am" || negations[base] || strings.HasSuffix(word, "n't") {
			return false
		}
	}
	return true
}

// pickIn is the offered issue a confirmation selects, or "". The reply's cues
// name it: its key, its place ("the first one", "the other") or words of its
// title and no other offered title's ("the wake word one"), read over every
// word. Cues that name two issues, a key not offered or a place past the
// offered issues pick none, whatever else the reply holds. A reply with a cue
// picks only when it is a selection (isSelection): nothing but its cues and
// words that frame a pick. Of one issue offered, a reply with no cue picks it
// when it is a yes form ("yes", "yeah, that one", "sure, that's it"). A key
// of one digit of an offered issue's project ("INFRA-7", "infra 7, please")
// is a key (offeredView), read as the turn's view reads one (oneDigitKey).
func pickIn(candidates []candidate, turn string) string {
	offered := offeredView(candidates)
	words := replyWords(offered.withoutOneDigitKeys(turn))
	cue := ""
	for _, named := range offered.issueKeys(turn) {
		if !slices.ContainsFunc(candidates, func(c candidate) bool { return c.key == named }) || (cue != "" && cue != named) {
			return ""
		}
		cue = named
	}
	named, conflict := placeOrTitle(candidates, words)
	switch {
	case conflict, named != "" && cue != "" && named != cue:
		return ""
	case named != "":
		cue = named
	}
	switch {
	case cue != "" && isSelection(candidates, words):
		return cue
	case cue == "" && len(candidates) == 1 && yesForm(words):
		return candidates[0].key
	}
	return ""
}

// isSelection reports whether words, a reply's words with its keys left out,
// hold nothing but cues and words that add nothing (selectionWords): a place
// (pickWords) or a word of an offered title. "the wake word one", "ABC-13,
// first please" and "yeah wake word" are selections; "first coffee, then
// lunch" is talk.
func isSelection(candidates []candidate, words []string) bool {
	titled := map[string]bool{}
	for _, candidate := range candidates {
		for _, word := range replyWords(candidate.title) {
			titled[word] = true
		}
	}
	for _, word := range words {
		if _, place := pickWords[word]; !place && !titled[word] && !selectionWords[word] {
			return false
		}
	}
	return true
}

// yesForm reports whether words are a yes alone: "yes", "yeah, that one",
// "sure, that's it", "that one", maybe with "please".
func yesForm(words []string) bool {
	phrase := strings.Join(words, " ")
	return yesWords[firstOf(words)] && yesTails[strings.Join(words[min(1, len(words)):], " ")] || yesPhrases[phrase]
}

// firstOf is words' first word, or "".
func firstOf(words []string) string {
	if len(words) == 0 {
		return ""
	}
	return words[0]
}

// placeOrTitle is the offered issue words name by its place (pickWords) or
// by words its title holds and no other offered title does, reading every
// word of both that names work (namingWords); "" when they name none.
// conflict says they name more than one, or a place past the offered issues
// ("the second one" of one).
func placeOrTitle(candidates []candidate, words []string) (key string, conflict bool) {
	pick := func(named string) bool {
		if key != "" && key != named {
			conflict = true
			return false
		}
		key = named
		return true
	}
	for _, word := range words {
		place, ok := pickWords[word]
		if !ok {
			continue
		}
		if place >= len(candidates) || !pick(candidates[place].key) {
			return "", true
		}
	}
	said := set(namingWords(strings.Join(words, " ")))
	titles := make([]map[string]bool, len(candidates))
	for i, candidate := range candidates {
		titles[i] = set(namingWords(candidate.title))
	}
	for i, candidate := range candidates {
		for word := range said {
			// A word every offered title holds tells two apart no better
			// than none; of one offered, every word of its title names it.
			if !titles[i][word] || (len(candidates) > 1 && !slices.ContainsFunc(titles, func(title map[string]bool) bool { return !title[word] })) {
				continue
			}
			if !pick(candidate.key) {
				return "", true
			}
			break
		}
	}
	return key, false
}
