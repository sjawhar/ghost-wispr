package calendar

import (
	"encoding/json"
	"errors"
	"io"
	"maps"
	"strconv"
	"strings"
)

// The bridge's record of an event: three private extended properties of the
// bot user's own copy of the occurrence (extendedProperties.private), which
// the calendar's owner may set.
const (
	doneProperty    = "ghostWisprDone"    // "true" once the event is done
	joinsProperty   = "ghostWisprJoins"   // the count of bots made for it, in decimal
	pendingProperty = "ghostWisprPending" // what the bridge still owes for it: a pendingValue in JSON, "{}" when nothing
)

// maxPropertyValue is the Calendar API's bound on a property's value: "The
// maximum size of a property's value is 1024 characters, and properties with
// longer values will be silently truncated" (Calendar API guide "Extended
// properties", Limits). maxBotID and maxSenderID bound the free fields of a
// pending record, its bots' ids (Attendee's ids are "bot_" and 16 letters
// and digits) and the echo's Meet sender id, so the longest record is 352
// characters.
const (
	maxPropertyValue = 1024
	maxBotID         = 64
	maxSenderID      = 128
)

// leaveNames name the leave reasons a pending record holds: the bridge's own
// leaves of the table of ends.
var leaveNames = map[BotEnd]string{
	EndLeaveRule:       "leave_rule",
	EndChatCheckFailed: "chat_check_failed",
	EndLinkLost:        "link_lost",
	EndSocketFailed:    "socket_failed",
}

// chatCheckFailed is the one result a pending record's chat_check holds: the
// Pi publishes call_not_recorded from a failed check alone; a passed check
// is recorded apart, as the bot whose check passed.
const chatCheckFailed = "failed"

// pendingValue is a Pending as ghostWisprPending holds it, for example
// {"leave":"link_lost","bot":"bot_Ab12Cd34Ef56Gh78","busy":true,"chat_check":"failed",
// "passed":"bot_Ab12Cd34Ef56Gh78","sender":"spaces/AAQAbc123/devices/42"}.
type pendingValue struct {
	Leave     string `json:"leave,omitempty"`
	Bot       string `json:"bot,omitempty"`
	Busy      bool   `json:"busy,omitempty"`
	ChatCheck string `json:"chat_check,omitempty"`
	Passed    string `json:"passed,omitempty"`
	Sender    string `json:"sender,omitempty"`
}

var errNotPending = errors.New("not a pending record")

// readRecord reads an event's record from its private properties. An unset
// count is 0, and an unset pending record owes nothing. A record nobody can
// read makes no new bot: a count that does not read as a whole number of at
// least 0 is MaxJoins, and so is the count beside a pending record that does
// not read as one, which then owes nothing, so nothing is replayed from it.
func readRecord(private map[string]string) (done bool, joins int, pending Pending) {
	done = private[doneProperty] == "true"
	joins = readJoins(private)
	v, ok := private[pendingProperty]
	if !ok {
		return done, joins, Pending{}
	}
	if pending, ok = decodePending(v); !ok {
		return done, MaxJoins, Pending{}
	}
	return done, joins, pending
}

// readJoins reads the count alone: unset is 0, and one that does not read as
// a whole number of at least 0 is MaxJoins.
func readJoins(private map[string]string) int {
	v, ok := private[joinsProperty]
	if !ok {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return MaxJoins
	}
	return n
}

// encodePending gives p as ghostWisprPending holds it. It refuses a record
// that is not one: a leave or a passed check with no bot or with a bot id of
// other than 1 to maxBotID letters, digits, "_" and "-", a bot with no leave,
// a reason that is not one of the bridge's leaves, or a sender with no
// passed check or of other than 1 to maxSenderID characters that JSON writes
// as they are; so every record it gives fits maxPropertyValue whole.
func encodePending(p Pending) (string, error) {
	if !validPending(p) {
		return "", errNotPending
	}
	v := pendingValue{Leave: leaveNames[p.Leave], Bot: p.Bot, Busy: p.Busy, Passed: p.Passed, Sender: p.Sender}
	if p.ChatCheckFailed {
		v.ChatCheck = chatCheckFailed
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// decodePending reads ghostWisprPending, and reports whether it is a record:
// one JSON object of pendingValue's keys alone, each with a value
// encodePending gives.
func decodePending(s string) (Pending, bool) {
	if !strings.HasPrefix(s, "{") {
		return Pending{}, false
	}
	dec := json.NewDecoder(strings.NewReader(s))
	dec.DisallowUnknownFields()
	var v pendingValue
	if err := dec.Decode(&v); err != nil {
		return Pending{}, false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Pending{}, false
	}
	p := Pending{Bot: v.Bot, Busy: v.Busy, ChatCheckFailed: v.ChatCheck == chatCheckFailed, Passed: v.Passed, Sender: v.Sender}
	if v.ChatCheck != "" && !p.ChatCheckFailed {
		return Pending{}, false
	}
	if v.Leave != "" {
		for kind, name := range leaveNames {
			if name == v.Leave {
				p.Leave = kind
			}
		}
		if p.Leave == 0 {
			return Pending{}, false
		}
	}
	return p, validPending(p)
}

func validPending(p Pending) bool {
	switch {
	case p.Passed == "" && p.Sender != "",
		p.Passed != "" && !validBotID(p.Passed),
		len(p.Sender) > maxSenderID || strings.IndexFunc(p.Sender, notLiteralRune) >= 0:
		return false
	case p.Leave == 0:
		return p.Bot == ""
	}
	_, ok := leaveNames[p.Leave]
	return ok && validBotID(p.Bot)
}

func validBotID(id string) bool {
	return id != "" && len(id) <= maxBotID && strings.IndexFunc(id, notIDRune) < 0
}

func notIDRune(r rune) bool {
	return (r < '0' || r > '9') && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && r != '-' && r != '_'
}

// notLiteralRune: a rune JSON does not write as it is (json.Marshal escapes
// control characters, '"', '\\', '<', '>' and '&', and non-ASCII may be
// long), so a sender of such runes could outgrow its bound once written.
func notLiteralRune(r rune) bool {
	return r <= ' ' || r >= 0x7f || strings.ContainsRune(`"\<>&`, r)
}

// withDone, withJoins and withPending give a copy of private with the
// record's value set. The copy keeps every other key, so a patch that sends
// it loses nothing whether the Calendar API merges a patch's map or replaces
// it.
func withDone(private map[string]string) map[string]string {
	out := copyPrivate(private)
	out[doneProperty] = "true"
	return out
}

func withJoins(private map[string]string, joins int) map[string]string {
	out := copyPrivate(private)
	out[joinsProperty] = strconv.Itoa(joins)
	return out
}

func withPending(private map[string]string, value string) map[string]string {
	out := copyPrivate(private)
	out[pendingProperty] = value
	return out
}

func copyPrivate(private map[string]string) map[string]string {
	out := make(map[string]string, len(private)+1)
	maps.Copy(out, private)
	return out
}
