package calendar

import (
	"maps"
	"strconv"
)

// The bridge's record of an event: two private extended properties of the
// bot user's own copy of the occurrence (extendedProperties.private), which
// the calendar's owner may set.
const (
	doneProperty  = "ghostWisprDone"  // "true" once the event is done
	joinsProperty = "ghostWisprJoins" // the count of bots made for it, in decimal
)

// readRecord reads an event's record from its private properties. An unset
// count is 0. A count that does not read as a whole number of at least 0 is
// MaxJoins, so a record nobody can read makes no new bot.
func readRecord(private map[string]string) (done bool, joins int) {
	done = private[doneProperty] == "true"
	v, ok := private[joinsProperty]
	if !ok {
		return done, 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return done, MaxJoins
	}
	return done, n
}

// withDone and withJoins give a copy of private with the record's value set.
// The copy keeps every other key, so a patch that sends it loses nothing
// whether the Calendar API merges a patch's map or replaces it.
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

func copyPrivate(private map[string]string) map[string]string {
	out := make(map[string]string, len(private)+1)
	maps.Copy(out, private)
	return out
}
