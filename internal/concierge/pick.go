package concierge

import (
	"slices"
	"strings"
	"time"

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
// key ("ABC 13"), by place ("the first one", "the other"), or by words of
// exactly one offered title ("the wake word one"). Of one issue offered, only
// a yes or its key picks it, and a no picks none (yesOrNo): side talk such as
// "the first item is rent" picks nothing. It returns the question first
// asked and the picked key. Either way the offer is used up: a turn that
// picks nothing is a question of its own.
func (c *Concierge) takePick(x *voice.Exchange, turn string) (asked, key string, ok bool) {
	c.mu.Lock()
	p, offered := c.picks[wakeKey(x)]
	delete(c.picks, wakeKey(x))
	c.mu.Unlock()
	if !offered {
		return "", "", false
	}
	if len(p.candidates) == 1 {
		switch yesOrNo(turn) {
		case "yes":
			return p.turn, p.candidates[0].key, true
		case "no":
			return p.turn, "", true
		}
		if slices.Contains(issueKeysIn(turn), p.candidates[0].key) {
			return p.turn, p.candidates[0].key, true
		}
		return p.turn, "", false
	}
	for _, named := range issueKeysIn(turn) {
		for _, candidate := range p.candidates {
			if candidate.key == named {
				return p.turn, named, true
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
	// yesWords and noWords answer "Did you mean KEY?"; replyWords may go
	// with either ("yeah, that one", "nope, not that one").
	yesWords   = set(strings.Fields("yes yeah yea yep yup sure correct right exactly ok okay"))
	noWords    = set(strings.Fields("no nope nah neither not"))
	replyWords = set(strings.Fields("that this one it is s the please uh um mm hmm oh i mean meant thanks thank you"))
)

// yesOrNo reads turn as a reply to a question about one issue: "yes" when it
// says yes and nothing but words that go with a reply, "no" likewise for no,
// else "".
func yesOrNo(turn string) string {
	yes, no := false, false
	for _, word := range strings.FieldsFunc(strings.ToLower(turn), func(r rune) bool { return r < 'a' || r > 'z' }) {
		switch {
		case yesWords[word]:
			yes = true
		case noWords[word]:
			no = true
		case !replyWords[word]:
			return ""
		}
	}
	switch {
	case yes && !no:
		return "yes"
	case no && !yes:
		return "no"
	}
	return ""
}
