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
	at        time.Time
	sessionID string
	turn      string
	keys      []string
}

// pickWords say which of the two offered issues a person picks.
var pickWords = map[string]int{
	"first": 0, "former": 0, "one": 0, "1st": 0,
	"second": 1, "latter": 1, "other": 1, "two": 1, "2nd": 1,
}

// wakeKey names an exchange by its wake alone: its session id may come only
// after the wake.
func wakeKey(x *voice.Exchange) string {
	return x.WakeAt.UTC().Format(time.RFC3339Nano)
}

// offerPick keeps, for x's next turn, the question turn and the issues the
// voice offered.
func (c *Concierge) offerPick(x *voice.Exchange, turn string, candidates []candidate) {
	keys := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		keys = append(keys, candidate.key)
	}
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, p := range c.picks {
		if now.Sub(p.at) > namedFor {
			delete(c.picks, key)
		}
	}
	c.picks[wakeKey(x)] = offeredPick{at: now, sessionID: x.SessionID, turn: turn, keys: keys}
}

// takePick reads turn as a pick of the issues x's last answer offered: by
// key ("ABC 13") or by place ("the first one", "the other"). It returns the
// question first asked and the picked key. Either way the offer is used up:
// a turn that picks nothing is a question of its own.
func (c *Concierge) takePick(x *voice.Exchange, turn string) (asked, key string, ok bool) {
	c.mu.Lock()
	p, offered := c.picks[wakeKey(x)]
	delete(c.picks, wakeKey(x))
	c.mu.Unlock()
	if !offered {
		return "", "", false
	}
	for _, named := range issueKeysIn(turn) {
		if slices.Contains(p.keys, named) {
			return p.turn, named, true
		}
	}
	for _, word := range strings.FieldsFunc(strings.ToLower(turn), func(r rune) bool { return !strings.ContainsRune("abcdefghijklmnopqrstuvwxyz0123456789", r) }) {
		if place, ok := pickWords[word]; ok && place < len(p.keys) {
			return p.turn, p.keys[place], true
		}
	}
	return "", "", false
}
