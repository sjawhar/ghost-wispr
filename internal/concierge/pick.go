package concierge

import (
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
// exactly one offered title ("the wake word one"). It returns the question
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
