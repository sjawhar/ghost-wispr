package concierge

import (
	"context"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// offeredPick is what an answer asked the room after it (R5): with no one
// clear match, the question asked and the two issues it named, best first;
// with a question that offered no issue ("What is its key?", or the model's
// own, askBack), the question asked and no issue.
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

// askBack keeps, for x's next turn, the question turn, after which the voice
// asked the room a question that offered no issue to pick: "What is its
// key?" (noIssueFound), or the model's own. The Supervisor takes the next
// utterance after any line that ends in "?" with no question mark of its own
// (R10), so that reply is read as a reply to an offer.
func (c *Concierge) askBack(x *voice.Exchange, turn string) {
	c.offerPick(x, turn, nil)
}

// takePick reads turn as a reply to the issues x's last answer offered
// (pick), and returns the question first asked and the picked key, "" for a
// whole no. Either way the offer is used up: a turn that picks nothing is a
// key said for the question (saysKey), a question of its own, or side talk
// (sideTalk).
func (c *Concierge) takePick(x *voice.Exchange, turn string) (asked, key string, ok bool) {
	c.mu.Lock()
	p, offered := c.picks[wakeKey(x)]
	delete(c.picks, wakeKey(x))
	c.mu.Unlock()
	if !offered {
		return "", "", false
	}
	if key, ok = p.pick(turn); ok {
		return p.turn, key, true
	}
	return "", "", false
}

// pick reads turn as a reply to p. A whole no ("no", "nope, not that one")
// picks none, ok. Any other reply that is no confirmation (confirms: a
// question mark, a question word or a negation) picks nothing, whatever key,
// place or title it names. A confirmation picks an issue only when it is a
// selection of one (pickIn): talk that holds a cue ("first coffee, then
// lunch") picks none. An offered issue is real, whatever the project list
// says: its key, said letter by letter ("A B C seven") or of one digit
// ("INFRA-7"), is a key of the reply. A no is a no whatever word and number
// of one digit it names: "no, not INFRA-7" (#194).
func (p *offeredPick) pick(turn string) (key string, ok bool) {
	view := offeredView(p.candidates)
	turn, _ = view.readSpelledKeys(turn)
	reply := view.withoutOneDigitKeys(turn)
	switch {
	case declines(withoutOneDigitWords(reply)):
		return "", true
	case !confirms(reply):
		return "", false
	}
	key = pickIn(p.candidates, turn)
	return key, key != ""
}

// pendingOffer is what x's last answer offered that no turn has replied to
// yet, and whether there is one.
func (c *Concierge) pendingOffer(x *voice.Exchange) (offeredPick, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, offered := c.picks[wakeKey(x)]
	return p, offered
}

// sideTalk reports whether turn, a reply to an offer that picks nothing
// (takePick), is side talk (Main's rule on R5's rewrite): it is no question
// (asks) and no key said alone (saysKey). The voice's question makes the
// next utterance a turn with no question mark, so talk in the room that only
// follows it must not be answered, read, searched or sent.
func sideTalk(turn string) bool {
	return !asks(turn) && !saysKey(turn)
}

// sideTalkOn reports whether turn, a reply to p, is side talk on any reading
// of it: the room's words, and vendor, the voice's own text of them
// (voice.Exchange.VendorReading), when vendor holds the room's words
// (holdsWords; Main, after Sol's fourth review of #191). A reading is side
// talk when it picks nothing (pick) and is sideTalk. Letters the view cannot
// tell are read as the key they may be (everySpelledKey). An endpoint can
// leave the room's words only the end of a statement, "can wait" of "okay,
// ABC-12 can wait", which on its own asks. A vendor's rephrasing, which does
// not hold the room's words, is no reading: a question it turned into a
// statement is still answered.
func (p *offeredPick) sideTalkOn(view projectsView, turn, vendor string) bool {
	readings := []string{turn}
	if holdsWords(vendor, turn) {
		readings = append(readings, vendor)
	}
	for _, reading := range readings {
		written, unread := view.readSpelledKeys(reading)
		if unread {
			written = everySpelledKey(written)
		}
		if _, picks := p.pick(written); !picks && sideTalk(written) {
			return true
		}
	}
	return false
}

// holdsWords reports whether text holds the words of part, together and in
// order, whatever their case, their punctuation or a key's spelling.
func holdsWords(text, part string) bool {
	words := func(s string) string { return " " + strings.Join(lowerWords(everySpelledKey(s)), " ") + " " }
	t, p := words(text), words(part)
	return strings.TrimSpace(p) != "" && strings.Contains(t, p)
}

// keyWords may frame a key said for "What is its key?", beside the words
// that frame a pick (selectionWords): "the key is ABC-12", "its number is
// 4592".
var keyWords = set(strings.Fields("key number is its"))

// saysKey reports whether turn, a reply to Wispr's question, says one key
// and nothing else of substance: an issue key ("ABC-12", "it's ABC-12",
// "ABC-12?") or a number alone ("4592"), with only the words that frame a
// pick or a key (selectionWords, keyWords), and no negation. "We spent 500
// on the voice" and "ABC-14 rollout" say no key: a bare key is no
// conversation, a key among other words may be.
func saysKey(turn string) bool {
	if _, negation := replyGate(turn); negation {
		return false
	}
	keys := issueKeysIn(turn)
	slices.Sort(keys)
	said := len(slices.Compact(keys))
	for _, word := range replyWords(turn) {
		switch {
		case len(word) >= 3 && strings.Trim(word, "0123456789") == "":
			said++ // a number alone, as bareIssueNumber reads one
		case !selectionWords[word] && !keyWords[word]:
			return false
		}
	}
	return said == 1
}

// takeKey reads reply, a key said alone (saysKey) for the question first
// asked, asked, after Wispr's question: "What is its key?", the model's own,
// or "Did you mean …?" when the key was not offered (Main on #191). A key of
// the turn's project snapshot (view), or the number of an agent that joined,
// is read: its target, the key alone, is what the answer reads for asked.
// Otherwise takeKey says what follows and reports done, with Answer's error:
// which issue a number names, offered for asked; that Dispatch is out of
// reach (R10), when the view cannot tell the key's project or Dispatch did
// not answer the number's reads; or that no issue was found, asking for the
// key of asked again.
func (c *Concierge) takeKey(ctx context.Context, x *voice.Exchange, view projectsView, asked, reply string, sp *speaker) (target string, done bool, err error) {
	if target, _, _ := strings.Cut(c.readTarget(view, reply), "|"); strings.HasPrefix(target, "keys:") {
		return target, false, nil
	}
	untold := view.keyUntold(reply)
	if len(issueKeysIn(reply)) == 0 {
		var candidates []candidate
		if candidates, untold = c.bareNumberCandidates(ctx, view, reply); len(candidates) > 0 {
			c.offerPick(x, asked, candidates)
			return "", true, sp.sayText(ctx, didYouMean(candidates), voice.Ack)
		}
	}
	if untold {
		if err := sp.sayText(ctx, cannotReachDispatch, voice.Ack); err != nil {
			return "", true, err
		}
		return "", true, errOutOfReach
	}
	c.askBack(x, asked)
	return "", true, sp.sayText(ctx, noIssueFound, voice.Ack)
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
	// selection. The list is kept narrow, and a phrase outside it picks
	// nothing by design ("let's do the first one", "go for the first one",
	// "I'd say the wake word one"): a missed pick is side talk, or a new
	// question when it asks one, where a wrong pick sends a question nobody
	// asked.
	selectionWords = set(strings.Fields(`uh um oh hmm mm okay ok so well yeah yea yes yep yup sure right correct exactly please thanks thank you
		the a one that that's this it it's issue ticket i i'll i'd mean meant use take pick choose go with want about
		think guess then maybe probably actually just now`))
)

// replyWords are turn's words, lower case, its issue keys and the fillers it
// begins with left out; an apostrophe stays in its word ("that's").
func replyWords(turn string) []string {
	words := lowerWords(compound.ReplaceAllString(spokenKey.ReplaceAllString(projectsView{}.spokenKeys(turn), " "), "realtime"))
	for len(words) > 0 && replyFillers[words[0]] {
		words = words[1:]
	}
	return words
}

// lowerWords are text's words, lower case, split at whatever is no letter,
// digit or apostrophe; an apostrophe stays in its word ("that's").
func lowerWords(text string) []string {
	return strings.FieldsFunc(strings.ToLower(strings.ReplaceAll(text, "’", "'")), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '\''
	})
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

// clauseOpeners may come before the word that starts a clause: "okay, what
// about the budget", "and when does it land".
var clauseOpeners = set(strings.Fields("uh um oh well hmm mm okay ok so and but then also right yeah yes actually"))

// asks reports whether turn is a question: it holds a question mark, or a
// clause of it starts, after the words that may open one (clauseOpeners),
// with a question word or a verb a question puts first (questionWords,
// auxiliaries, "am"; "what's" is "what", "isn't" is "is"). Inside a clause
// such a word asks nothing: "we can move on", "let's see what it is". An
// issue key is a word of its clause, so "okay, ABC-12 can wait" asks
// nothing. After an offer, a turn that picks nothing and asks nothing is
// side talk (sideTalk). The gate before a pick (replyGate) reads every word
// instead: there a wrong question costs only a pick.
func asks(turn string) bool {
	if strings.Contains(turn, "?") {
		return true
	}
	for _, clause := range strings.FieldsFunc(turn, func(r rune) bool { return strings.ContainsRune(",;:.!", r) }) {
		for _, word := range lowerWords(clause) {
			if clauseOpeners[word] {
				continue
			}
			base, _, _ := strings.Cut(word, "'")
			switch word {
			case "can't":
				base = "can"
			case "won't":
				base = "will"
			default:
				if verb, ok := strings.CutSuffix(word, "n't"); ok {
					base = verb
				}
			}
			if questionWords[base] || auxiliaries[base] || base == "am" {
				return true
			}
			break
		}
	}
	return false
}

// confirms reports whether turn may confirm an offered issue: it asks
// nothing and holds no negation (replyGate), anywhere: "the first one, do I
// pick that" asks. Every way a reply picks an issue (pickIn) runs only behind
// it.
func confirms(turn string) bool {
	question, negation := replyGate(turn)
	return !question && !negation
}

// replyGate reads turn for the gate before every pick (#187): question, for
// a question mark, or a question word or a verb a question puts first
// (questionWords, auxiliaries, "am"; "what's" is "what") anywhere in it;
// negation, for a negation (negations, or a word that ends in "n't").
func replyGate(turn string) (question, negation bool) {
	question = strings.Contains(turn, "?")
	for _, word := range replyWords(turn) {
		base, _, _ := strings.Cut(word, "'")
		question = question || questionWords[base] || auxiliaries[base] || base == "am"
		negation = negation || negations[base] || strings.HasSuffix(word, "n't")
	}
	return question, negation
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
