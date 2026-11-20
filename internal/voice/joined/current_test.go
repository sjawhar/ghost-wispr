package joined

import (
	"context"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/envoy"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// The tests here check that every effect is validated at the moment it
// happens, against the roster's live state, never against a snapshot its
// caller took earlier: a hand the Supervisor is about to speak, the keyterms,
// a welcome.

// pausingFloor is the real Supervisor behind a floor that can hold a raise
// before it reaches the Supervisor and a check after it returns, and can
// wrap each raised hand's Current.
type pausingFloor struct {
	next        Floor
	beforeRaise func()
	afterRaise  func()
	beforeLower func()
	wrapCurrent func(current func() bool) func() bool
}

func (f *pausingFloor) RaiseHand(h *voice.Hand) error {
	if f.beforeRaise != nil {
		f.beforeRaise()
	}
	if f.wrapCurrent != nil {
		h.Current = f.wrapCurrent(h.Current)
	}
	err := f.next.RaiseHand(h)
	if f.afterRaise != nil {
		f.afterRaise()
	}
	return err
}

func (f *pausingFloor) LowerHands(membership string) {
	if f.beforeLower != nil {
		f.beforeLower()
	}
	f.next.LowerHands(membership)
}

// TestAnEndedMembershipsHandIsNeverSpoken fails if a floor request whose raise
// reaches the Supervisor only after its agent's leave completed, lowering
// included, is spoken on "Wispr, go ahead" before the roster's own check
// after the raise lowers it. (ReviewPR122b's
// TestReviewFEndedMembershipCannotSpeakDuringRaiseRecheck.)
func TestAnEndedMembershipsHandIsNeverSpoken(t *testing.T) {
	sup := newSupervisorRig(t)
	raiseEntered, raiseRelease := make(chan struct{}), make(chan struct{})
	checkEntered, checkRelease := make(chan struct{}), make(chan struct{})
	floor := &pausingFloor{
		next:        sup.s,
		beforeRaise: func() { close(raiseEntered); <-raiseRelease },
		afterRaise:  func() { close(checkEntered); <-checkRelease },
	}
	r := newRig(t, func(cfg *Config) {
		cfg.Now = nil
		cfg.Floor = floor
		cfg.Session = func() string { return "sess-1" }
	})
	r.clock.set(time.Now())
	r.join(t, "agent-1", "ABC-12")
	sup.mu.Lock()
	sup.handDone = r.roster.HandDone
	sup.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = r.roster.Deliver(context.Background(), &envoy.Message{EventID: "delayed-floor", SourceSession: "agent-1", Text: "ghost-wispr floor\nThe release is ready.", IssuedAt: r.clock.Now()})
	}()
	<-raiseEntered
	r.deliver(t, "agent-1", "ghost-wispr leave")
	if len(r.roster.Joined()) != 0 {
		t.Fatal("control: the leave did not end the membership")
	}
	close(raiseRelease)
	<-checkEntered // the hand is up on the Supervisor; the roster's check has not run
	sup.heard("Wispr, go ahead.")
	var spoken string
	select {
	case spoken = <-sup.says:
	case <-time.After(300 * time.Millisecond):
	}
	close(checkRelease)
	<-done
	r.roster.wg.Wait()
	if spoken != "" {
		t.Fatalf("the hand of a membership whose leave had completed was spoken: %q", spoken)
	}
}

// TestAStaleEndCannotOverwriteNewerKeyterms fails if a meeting's end, which
// read the recording session before the next meeting began and stalled, then
// gives the live stream the keyterms of that stale read, overwriting the next
// meeting's joined key; or if an older numbered list lands after a newer one.
// (ReviewPR122b's TestReviewFOldMeetingEndCannotOverwriteNewMeetingsKeys and
// TestReviewFKeytermOldApplyCannotOverwriteNewer.)
func TestAStaleEndCannotOverwriteNewerKeyterms(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var hold atomic.Bool
	r := newRig(t, func(cfg *Config) {
		session := cfg.Session
		cfg.Session = func() string {
			cur := session()
			if hold.CompareAndSwap(true, false) {
				close(entered)
				<-release
			}
			return cur
		}
	})
	r.join(t, "agent-1", "ABC-12")
	r.setSession("") // the recorder clears its session before it calls the end hook
	hold.Store(true)
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.roster.SessionEnded("sess-1")
	}()
	select {
	case <-entered:
	case <-time.After(100 * time.Millisecond):
		// The end did not read the session at all: nothing to hold.
		hold.Store(false)
	}
	r.setSession("sess-2")
	r.join(t, "agent-2", "XYZ-9")
	close(release)
	<-done
	r.roster.wg.Wait()
	if got := r.roster.Joined(); len(got) != 1 || got[0].Agent != "agent-2" || got[0].SessionID != "sess-2" {
		t.Fatalf("control: the next meeting's membership is missing: %+v", got)
	}
	if got := r.notes.lastKeyterms(); !slices.Equal(got, []string{"XYZ-9"}) {
		t.Fatalf("keyterms %q after the stale end, want the next meeting's [XYZ-9]", got)
	}

	// An older numbered list never lands after a newer one.
	r.roster.mu.Lock()
	older := r.roster.keytermsLocked()
	r.roster.mu.Unlock()
	r.join(t, "agent-3", "INFRA-1027")
	newer := r.notes.lastKeyterms()
	r.roster.applyKeyterms(older)
	if got := r.notes.lastKeyterms(); !slices.Equal(got, newer) {
		t.Fatalf("an older list overwrote a newer one: got %q, want %q", got, newer)
	}
}

// TestHandCurrentCannotDeadlockWithTheRoster fails if the roster holds its
// lock while it waits on the Supervisor: the Supervisor asks a hand's Current,
// which takes the roster's lock, while it holds its own, so a roster that
// raised or lowered a hand under its lock would deadlock with it. Here the
// Supervisor is held inside Current while the roster raises a hand and ends a
// membership; each must wait on the Supervisor with the roster's lock free.
func TestHandCurrentCannotDeadlockWithTheRoster(t *testing.T) {
	sup := newSupervisorRig(t)
	inCurrent, leaveCurrent := make(chan struct{}), make(chan struct{})
	var holdOnce, abort atomic.Bool
	holdOnce.Store(true)
	floor := &pausingFloor{
		next: sup.s,
		wrapCurrent: func(current func() bool) func() bool {
			return func() bool {
				if holdOnce.CompareAndSwap(true, false) {
					close(inCurrent)
					<-leaveCurrent
				}
				if abort.Load() {
					return false // the roster's lock is held: asking it now would deadlock the test
				}
				return current()
			}
		},
	}
	r := newRig(t, func(cfg *Config) {
		cfg.Now = nil
		cfg.Floor = floor
		cfg.Session = func() string { return "sess-1" }
	})
	r.clock.set(time.Now())
	r.join(t, "agent-1", "ABC-12")
	r.join(t, "agent-2", "XYZ-9")
	r.deliver(t, "agent-1", "ghost-wispr floor\nThe release is ready.")
	heard := make(chan struct{})
	go func() {
		defer close(heard)
		sup.heard("Wispr, go ahead.") // the Supervisor asks Current under its own lock
	}()
	select {
	case <-inCurrent:
	case <-time.After(testWait):
		t.Fatal("control: the Supervisor never asked the hand whether its membership is current")
	}
	// The roster raises a hand, then ends a membership whose hand is up, while
	// the Supervisor is held in Current: each waits on the Supervisor, and
	// must hold no lock of the roster's while it does.
	lockFree := func(what string) {
		t.Helper()
		deadline := time.Now().Add(150 * time.Millisecond)
		for time.Now().Before(deadline) {
			if !r.roster.mu.TryLock() {
				time.Sleep(time.Millisecond)
				if !r.roster.mu.TryLock() {
					abort.Store(true)
					close(leaveCurrent)
					t.Fatalf("the roster held its lock while %s waited on the Supervisor, which waits on the roster's lock in Current: a deadlock", what)
				}
			}
			r.roster.mu.Unlock()
			time.Sleep(5 * time.Millisecond)
		}
	}
	raising, lowering := make(chan struct{}), make(chan struct{})
	floor.beforeRaise = func() { close(raising) }
	floor.beforeLower = func() { close(lowering) }
	raised, left := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(raised)
		_, _ = r.roster.Deliver(context.Background(), &envoy.Message{EventID: "second-floor", SourceSession: "agent-2", Text: "ghost-wispr floor\nThe keys rotated.", IssuedAt: time.Now()})
	}()
	select {
	case <-raising:
	case <-time.After(testWait):
		t.Fatal("control: the second floor request never reached the Supervisor")
	}
	lockFree("a raise")
	go func() {
		defer close(left)
		_, _ = r.roster.Deliver(context.Background(), &envoy.Message{EventID: "leave-1", SourceSession: "agent-1", Text: "ghost-wispr leave", IssuedAt: time.Now()})
	}()
	select {
	case <-lowering:
	case <-time.After(testWait):
		t.Fatal("control: the leave never lowered its hand on the Supervisor")
	}
	lockFree("a lower")
	close(leaveCurrent)
	for name, ch := range map[string]chan struct{}{"the go-ahead": heard, "the floor request": raised, "the leave": left} {
		select {
		case <-ch:
		case <-time.After(testWait):
			t.Fatalf("%s did not finish: a deadlock", name)
		}
	}
	r.roster.wg.Wait()
}

// TestNoWelcomeAfterALeave fails if a join's welcome goes out after the
// membership it welcomes ended, here by a leave that came while the join was
// giving the live stream its keyterms.
func TestNoWelcomeAfterALeave(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var hold atomic.Bool
	r := newRig(t, func(cfg *Config) {
		keyterms := cfg.Keyterms
		cfg.Keyterms = func(keys []string) {
			if hold.CompareAndSwap(true, false) {
				close(entered)
				<-release
			}
			keyterms(keys)
		}
	})
	hold.Store(true)
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		_, _ = r.roster.Deliver(context.Background(), &envoy.Message{EventID: "join", SourceSession: "agent-1", Text: "ghost-wispr join ABC-12", IssuedAt: r.clock.Now()})
	}()
	<-entered
	left := make(chan struct{})
	go func() {
		defer close(left)
		_, _ = r.roster.Deliver(context.Background(), &envoy.Message{EventID: "leave", SourceSession: "agent-1", Text: "ghost-wispr leave", IssuedAt: r.clock.Now()})
	}()
	eventually(t, "the leave ending the membership", func() bool { return len(r.roster.Joined()) == 0 })
	close(release)
	<-joined
	<-left
	r.roster.wg.Wait()
	for _, m := range r.agents.to("agent-1") {
		if strings.HasPrefix(m.Message, "You joined") {
			t.Fatalf("the join was welcomed after its membership ended: %q", m.Message)
		}
	}
}
