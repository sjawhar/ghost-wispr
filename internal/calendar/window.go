package calendar

import "time"

// The times of the spec's "The calendar: the watcher and the bridge" and its
// Limits ("The pool's lead", "The pool's hold", "The bot's creation", "New
// bots").
const (
	PoolLead   = 10 * time.Minute // the pool is up from this long before the start
	PoolHold   = 4 * time.Hour    // until done, or this long after the end
	CreateLead = 5 * time.Minute  // a bot is made when the start is at most this far away
	JoinAtMin  = 60 * time.Second // join_at is set only when the start is more than this far away
	MaxJoins   = 4                // the first bot and at most 3 new bots
)

// KeepsPoolUp reports whether e keeps the call pool up at now: from PoolLead
// before its start until it is done, or until PoolHold after its end,
// whichever comes first. An event that owes the Pi a control (a busy, or a
// failed chat_check, the link has not taken) keeps it up also while it is
// done, until PoolHold after its end, so the bridge sends it once the link is
// back and the Pi publishes call_not_recorded (R2, R14).
//
//nolint:gocritic // hugeParam: the contract passes Event by value
func KeepsPoolUp(e Event, now time.Time) bool {
	return (!e.Done || e.Pending.Owes()) && !now.Before(e.Start.Add(-PoolLead)) && now.Before(e.End.Add(PoolHold))
}
