package concierge

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"google.golang.org/genai"
)

// heldLog is a log handler that holds the record whose message is match
// until release closes; entered closes when that record comes. Other records
// pass.
type heldLog struct {
	match            string
	entered, release chan struct{}
	once             sync.Once
}

func newHeldLog(match string) *heldLog {
	return &heldLog{match: match, entered: make(chan struct{}), release: make(chan struct{})}
}

func (h *heldLog) Enabled(context.Context, slog.Level) bool { return true }

//nolint:gocritic // slog.Handler's Handle takes the record by value.
func (h *heldLog) Handle(_ context.Context, r slog.Record) error {
	if r.Message == h.match {
		h.once.Do(func() { close(h.entered) })
		<-h.release
	}
	return nil
}

func (h *heldLog) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *heldLog) WithGroup(string) slog.Handler      { return h }

// lockFreeWhileHeld waits for h's record, reports on free whether c.mu could
// be taken within a second while that record's write waits, then releases
// it. Run it on a goroutine of its own.
func lockFreeWhileHeld(c *Concierge, h *heldLog, free chan<- bool) {
	defer close(h.release)
	select {
	case <-h.entered:
	case <-time.After(5 * time.Second):
		free <- false
		return
	}
	got := make(chan struct{})
	go func() {
		defer close(got)
		c.mu.Lock() // taking the lock is the check
		defer c.mu.Unlock()
	}()
	select {
	case <-got:
		free <- true
	case <-time.After(time.Second):
		free <- false
	}
}

// TestNoConciergeLogWriteHoldsItsLock fails if the Concierge writes a log
// record while it holds its lock (c.mu): a log write can wait, on a journal
// that does not read, and every call of the Concierge's, its stop's
// included, would wait behind it. The cases are the two records the audit of
// #132 (review 6006047326) found written under the lock: a failed read of
// Dispatch's projects, and an answer that searches again because its words
// do not hold the read-ahead's.
func TestNoConciergeLogWriteHoldsItsLock(t *testing.T) {
	t.Run("a failed read of the projects", func(t *testing.T) {
		held := newHeldLog("concierge could not read Dispatch's projects")
		r := newLoggedRig(t, slog.New(held))
		r.records.mu.Lock()
		r.records.projects = nil // the next read fails
		r.records.mu.Unlock()
		r.concierge.mu.Lock()
		r.concierge.projectsRead = time.Time{} // the list is stale
		r.concierge.mu.Unlock()
		free := make(chan bool, 1)
		go lockFreeWhileHeld(r.concierge, held, free)
		r.concierge.isProject("ABC") // reads the stale list in the background
		if !<-free {
			t.Fatal("the Concierge's lock was held while it logged the failed read of the projects")
		}
	})
	t.Run("an answer that searches again", func(t *testing.T) {
		held := newHeldLog("concierge searches again: the turn's search words are not the read-ahead's")
		r := newLoggedRig(t, slog.New(held))
		r.concierge.searchSettle = 50 * time.Millisecond
		unclearSearch(r)
		r.records.hits[0].Score = 0.012
		r.concierge.Heard(context.Background(), &r.exchange, "what is happening with the realtime voice rollout")
		eventually(t, "the read-ahead searched", func() bool { return len(r.records.searches()) == 1 })
		r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("PR 80 is in review."))}}
		free := make(chan bool, 1)
		go lockFreeWhileHeld(r.concierge, held, free)
		answerTurn(t, r, "what is happening with the realtime voice")
		if !<-free {
			t.Fatal("the Concierge's lock was held while it logged that the answer searches again, or it did not log it")
		}
	})
}
