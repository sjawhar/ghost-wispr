package concierge

import (
	"context"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/sjawhar/ghost-wispr/internal/voice"
	"github.com/sjawhar/ghost-wispr/internal/wake/transcript"
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

// sideTalkOn reports whether said, what the room's reply to p says after its
// last take-back (replyText), is side talk: it is nothing, or it picks
// nothing (pick) and is sideTalk. Letters the view cannot tell are read as
// the key they may be (everySpelledKey). The reply is every final the room
// said since the voice asked (voice.Exchange.Reply), so an endpoint inside a
// statement cannot leave it only the statement's end, "can wait" of "okay,
// ABC-12 can wait", which on its own asks; and a reply that holds a question
// anywhere is a question, unless it then takes it back (Main, after Sol's
// fifth review of #191). A vendor's text is never read: it could neither be
// trusted to make a reply side talk nor to make it a question.
func (p *offeredPick) sideTalkOn(view projectsView, said string) bool {
	if said == "" {
		return true
	}
	written, unread := view.readSpelledKeys(said)
	if unread {
		written = everySpelledKey(written)
	}
	_, picks := p.pick(written)
	return !picks && sideTalk(written)
}

// readsReply reports whether the answer to turn, a reply to p that is not
// side talk, reads said, what the room's whole reply says after its last
// take-back (replyText), rather than turn: the turn is the reply's last part
// and picks nothing, says no key and asks nothing, while the reply asks or
// says a key alone, which was said before the turn or across its start.
// "What about the budget?" then "okay, sure" is answered as the question,
// not as "okay, sure" (Main, before Sol's sixth review of #191); "ABC
// forty", an endpoint, then "two" is the key ABC-42, not "two" (Sol's
// seventh review). Letters the view can tell are read as their key first.
func (p *offeredPick) readsReply(view projectsView, turn, said string) bool {
	if said == "" || asks(turn) || saysKey(turn) {
		return false
	}
	if _, picks := p.pick(turn); picks {
		return false
	}
	read, _ := view.readSpelledKeys(said)
	return asks(read) || saysKey(read)
}

// replyText is what reply, the room's finals one a line, says after its
// last take-back (afterTakeBack), found on the finals as Deepgram wrote
// them; then joined as a turn is, and its keys written over the whole
// (writeKeys), so a key split across finals ("ABC forty" / "two") is one key
// (Main, after Sol's seventh review of #191). Nothing said before the
// take-back is in it: it is what the side-talk decision, the answer and any
// question to a holder read.
func (v projectsView) replyText(reply string) string {
	return v.writeKeys(strings.Join(strings.Fields(afterTakeBack(reply)), " "))
}

// writeKeys writes the keys of the view's projects said in a spoken form
// ("ABC forty two") as keys, as the Supervisor writes a turn's
// (transcript.WriteKeys). With no project known it writes none.
func (v projectsView) writeKeys(text string) string {
	if text == "" || len(v.known) == 0 {
		return text
	}
	projects := make([]string, 0, len(v.known))
	for project := range v.known {
		projects = append(projects, project)
	}
	return transcript.WriteKeys(text, projects)
}

// takeBacks are the clauses that take back what was said before them, as
// "Wispr, never mind" does, after the words that may open a clause.
var takeBacks = set([]string{"stop", "never mind", "nevermind", "cancel", "forget it", "enough", "that's all", "thats all"})

// afterTakeBack is what reply says after its last take-back: a clause (cut
// at a stop, a comma or a line; each final of the room's reply is a line,
// voice.Exchange.Reply) that, after the words that may open one
// (clauseOpeners, replyFillers, a no: "okay", "actually", "no", "uh", "so",
// "well"), is exactly a take-back phrase (takeBacks) and nothing else, and
// ends in no question mark. A take-back cancels what was said before it:
// "what about ABC-12? okay, never mind" says nothing after it, and answers
// nothing, as "Wispr, never mind" answers nothing; "never mind. what about
// ABC-12?" says the question. A clause with any other word, a question word
// or a question mark is no take-back: "stop the rollout?", "never mind the
// budget what about ABC-12?" (Sol's sixth review of #191, where a prefix
// match took them for take-backs). Nor is a clause that holds a quote mark:
// a quoted word is what the room is talking about, `did ABC-12, "Cancel",
// ship?` (Sol's tenth review of #191). With no take-back, it is reply.
func afterTakeBack(reply string) string {
	rest := reply
	for clause := reply; clause != ""; {
		end := strings.IndexAny(clause, ",;:.!?\n")
		next := ""
		if end < 0 {
			end = len(clause)
		} else {
			next = clause[end+1:]
		}
		if (end == len(clause) || clause[end] != '?') && !quotes(clause[:end]) && takesBack(clause[:end]) {
			rest = next
		}
		clause = next
	}
	return strings.TrimSpace(rest)
}

// takesItBack reports whether reply, the room's finals one a line, holds a
// take-back (afterTakeBack). Found on the raw finals, a take-back that the
// end of a final ends is a clause of its own; in a turn the Supervisor joined
// with spaces it is not ("what about ABC-42? never mind what about ABC-12?"),
// so the answer reads the raw cut (replyText), never the joined turn (Sol's
// eighth review of #191).
func takesItBack(reply string) bool {
	return afterTakeBack(reply) != strings.TrimSpace(reply)
}

// TakeBack is the room's last take-back in finals, one a line, keys
// unwritten, as the answer finds it (afterTakeBack): taken reports one, and
// rest is what the finals say after it. The Supervisor drops what it
// cancelled from the room's recent words (voice.Config.TakeBack).
func TakeBack(finals string) (rest string, taken bool) {
	rest = afterTakeBack(finals)
	return rest, rest != strings.TrimSpace(finals)
}

// quotes reports whether clause holds a quote mark, straight or curly: a
// double one anywhere, or a single one that opens or closes a word, not the
// apostrophe inside one ("that's all").
func quotes(clause string) bool {
	if strings.ContainsAny(clause, "\"“”„«»") {
		return true
	}
	for _, word := range strings.Fields(clause) {
		for _, mark := range []string{"'", "‘", "’"} {
			if strings.HasPrefix(word, mark) || strings.HasSuffix(word, mark) {
				return true
			}
		}
	}
	return false
}

// roomFinals is the room's raw words that a take-back is found on: the reply
// to Wispr's question (voice.Exchange.Reply), else the turn's own finals
// (voice.Exchange.Finals) when turn is that turn (AskerTurn), one a line,
// else turn. A vendor's new question, asked before the room's next words,
// is never cut by the take-back of the turn before (Sol's tenth review of
// #191).
func roomFinals(x *voice.Exchange, turn string) string {
	if strings.TrimSpace(x.Reply) != "" {
		return x.Reply
	}
	if strings.TrimSpace(x.Finals) != "" && strings.TrimSpace(turn) == strings.TrimSpace(x.AskerTurn) {
		return x.Finals
	}
	return turn
}

// takesBack reports whether clause, with no question mark, is a take-back
// phrase after the words that may open a clause (afterTakeBack).
func takesBack(clause string) bool {
	words := lowerWords(clause)
	for len(words) > 0 && (clauseOpeners[words[0]] || replyFillers[words[0]] || noWords[words[0]]) {
		words = words[1:]
	}
	return len(words) > 0 && takeBacks[strings.Join(words, " ")]
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
