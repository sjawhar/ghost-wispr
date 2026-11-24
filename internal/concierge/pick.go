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

// takePick reads turn as a pick of the issues x's last answer offered: by
// key, when the reply is the key alone or with "please" or "that one", by
// place ("the first one", "the other"), or by words of exactly one offered
// title ("the wake word one"). Of one issue offered, only a whole reply that
// confirms picks it: a yes form or its key; a no picks none, whatever was
// offered (replyTo). Side talk, a question or a new question that names the
// key picks nothing. It returns the question first asked and the picked key.
// Either way the offer is used up: a turn that picks nothing is a question
// of its own.
func (c *Concierge) takePick(x *voice.Exchange, turn string) (asked, key string, ok bool) {
	c.mu.Lock()
	p, offered := c.picks[wakeKey(x)]
	delete(c.picks, wakeKey(x))
	c.mu.Unlock()
	if !offered {
		return "", "", false
	}
	kind := replyTo(turn)
	if kind == "no" {
		return p.turn, "", true
	}
	if len(p.candidates) == 1 {
		switch {
		case kind == "yes":
			return p.turn, p.candidates[0].key, true
		case kind == "key" && slices.Contains(issueKeysIn(turn), p.candidates[0].key):
			return p.turn, p.candidates[0].key, true
		}
		return p.turn, "", false
	}
	if kind == "key" {
		for _, named := range issueKeysIn(turn) {
			for _, candidate := range p.candidates {
				if candidate.key == named {
					return p.turn, named, true
				}
			}
		}
	}
	for _, word := range strings.FieldsFunc(strings.ToLower(turn), func(r rune) bool { return !strings.ContainsRune("abcdefghijklmnopqrstuvwxyz0123456789", r) }) {
		if place, ok := pickWords[word]; ok && place < len(p.candidates) {
			return p.turn, p.candidates[place].key, true
		}
	}
	said := set(strings.Fields(workWords(turn)))
	// Words every offered title holds tell them apart no better than none.
	for word := range said {
		shared := true
		for _, candidate := range p.candidates {
			if !set(strings.Fields(workWords(candidate.title)))[word] {
				shared = false
				break
			}
		}
		if shared {
			delete(said, word)
		}
	}
	picked := ""
	for _, candidate := range p.candidates {
		for _, word := range strings.Fields(workWords(candidate.title)) {
			if said[word] {
				if picked != "" {
					return "", "", false // words of both titles
				}
				picked = candidate.key
				break
			}
		}
	}
	if picked == "" {
		return "", "", false
	}
	return p.turn, picked, true
}

var (
	// replyFillers may begin a reply: "uh, yes".
	replyFillers = set(strings.Fields("uh um oh well hmm mm"))
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
	// keyTails may follow an issue key that picks it: "ABC-12, please".
	keyTails = set([]string{"", "please", "that one", "the one", "yes", "yes please"})
)

// replyTo reads turn as a whole reply to "Did you mean …?": "yes", "no", or
// "key" when it is an issue key alone or with "please" or "that one". Words
// a reply begins with ("uh") do not count. Anything else is "", a new
// question: a question mark, a number or any other word makes it one, so
// "Is that right?", "That's 100, right." and "What is blocking ABC-12" are
// questions, and "No, not ABC-12." is a no.
func replyTo(turn string) string {
	if strings.Contains(turn, "?") {
		return ""
	}
	keyed := len(issueKeysIn(turn)) > 0
	rest := spokenKey.ReplaceAllString(spokenKeys(turn), " ")
	words := strings.FieldsFunc(strings.ToLower(strings.ReplaceAll(rest, "’", "'")), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '\''
	})
	for len(words) > 0 && replyFillers[words[0]] {
		words = words[1:]
	}
	if len(words) == 0 {
		if keyed {
			return "key"
		}
		return ""
	}
	phrase, first, after := strings.Join(words, " "), words[0], strings.Join(words[1:], " ")
	switch {
	case noWords[first] && noTails[after], noPhrases[phrase]:
		return "no"
	case keyed && keyTails[phrase]:
		return "key"
	case keyed:
		return ""
	case yesWords[first] && yesTails[after], yesPhrases[phrase]:
		return "yes"
	}
	return ""
}
