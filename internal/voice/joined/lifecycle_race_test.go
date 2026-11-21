package joined

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/envoy"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// The tests here hold a lifecycle step (a deletion, the voice stopping, a
// leave) inside its slow cleanup, and run a send, a join or a raise against
// it: nothing may act for a membership or a meeting whose end has begun.

// TestATickCannotSendForAMeetingBeingDeleted fails if a queued pointer goes
// out at a tick while a deleted meeting's cleanup is still lowering another
// agent's hands. (ReviewPR122b's
// TestReviewDTickCannotSendWhileDeletionDrainsMembers.)
func TestATickCannotSendForAMeetingBeingDeleted(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "unit.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.CreateSession("sess-1", meetingStart); err != nil {
		t.Fatal(err)
	}
	r := newRig(t, func(cfg *Config) {
		cfg.Started = func(id string) time.Time {
			session, err := store.GetSession(id)
			if err != nil {
				return time.Time{}
			}
			return session.StartedAt
		}
	})
	r.join(t, "agent-1", "ABC-12")
	r.join(t, "agent-2", "XYZ-9")
	r.roster.OnSegment(r.segment("ABC-12 and XYZ-9 are late", false))
	r.roster.wg.Wait()
	if len(r.pointers("agent-1")) != 1 || len(r.pointers("agent-2")) != 1 {
		t.Fatal("control: the first pointers were not sent")
	}
	r.clock.advance(15 * time.Second)
	r.roster.OnSegment(r.segment("ABC-12 and XYZ-9 are late", false)) // queued for both
	r.clock.advance(45 * time.Second)
	if err := store.EndSession("sess-1", r.clock.Now(), "", ""); err != nil {
		t.Fatal(err)
	}
	r.setSession("")
	if _, err := store.DeleteSession("sess-1", t.TempDir()); err != nil {
		t.Fatal(err)
	}

	var first atomic.Value
	lowerEntered, lowerRelease := make(chan struct{}), make(chan struct{})
	r.floor.beforeLower = func(agent string) {
		if first.CompareAndSwap(nil, agent) {
			close(lowerEntered)
			<-lowerRelease
		}
	}
	deleteDone := make(chan struct{})
	go func() {
		defer close(deleteDone)
		r.roster.SessionsDeleted([]string{"sess-1"})
	}()
	<-lowerEntered
	other := map[string]string{"agent-1": "agent-2", "agent-2": "agent-1"}[first.Load().(string)]
	r.roster.Tick()
	r.roster.wg.Wait()
	sent := len(r.pointers(other))
	close(lowerRelease)
	<-deleteDone
	if sent != 1 {
		t.Fatalf("a tick sent %s a queued pointer to a deleted meeting while its cleanup ran", other)
	}
}

// TestAJoinInFlightCannotOutliveTheVoiceStopping fails if a join still in
// its reads when the voice stops (CloseAgents) commits after the stop
// returned, even once another Envoy session opens. (ReviewPR122b's
// TestReviewDPendingJoinCannotOutliveVoiceStop.)
func TestAJoinInFlightCannotOutliveTheVoiceStopping(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	r := newRig(t, func(cfg *Config) {
		cfg.Title = func(context.Context, string) (string, error) {
			close(entered)
			<-release
			return titles["ABC-12"], nil
		}
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = r.roster.Deliver(context.Background(), &envoy.Message{EventID: "join-in-flight", SourceSession: "agent-1", Text: "ghost-wispr join ABC-12", IssuedAt: r.clock.Now()})
	}()
	<-entered
	r.roster.CloseAgents(context.Background())
	r.roster.SetAgents(r.agents) // the next Envoy session, which the join did not begin under
	close(release)
	<-done
	r.roster.wg.Wait()
	if got := r.roster.Joined(); len(got) != 0 {
		t.Fatalf("a join that began before the voice stopped committed after it: %+v", got)
	}
}

// TestAnOldLeaveCannotLowerANewMembershipsHand fails if the end of an
// agent's membership (its leave, or its meeting's end), still lowering its
// hands when the same Envoy session joins again (the same meeting, or the
// next one) and asks for the floor, takes down the new membership's hand or
// the new membership. (ReviewPR122b's
// TestReviewDOldLeaveCannotLowerNewMembershipsHand, and its verdict on
// d6a069b: an old meeting's cleanup removed a new meeting's hand.)
func TestAnOldLeaveCannotLowerANewMembershipsHand(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  func(r *rig)
		next string // the meeting the agent joins again
	}{
		{"a leave, then a join of the same meeting", func(r *rig) {
			_, _ = r.roster.Deliver(context.Background(), &envoy.Message{EventID: "old-leave", SourceSession: "agent-1", Text: "ghost-wispr leave", IssuedAt: r.clock.Now()})
		}, "sess-1"},
		{"the meeting's end, then a join of the next meeting", func(r *rig) {
			r.roster.SessionEnded("sess-1")
		}, "sess-2"},
		{"the meeting's deletion, then a join of the next meeting", func(r *rig) {
			r.roster.SessionsDeleted([]string{"sess-1"})
		}, "sess-2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, nil)
			r.join(t, "agent-1", "ABC-12")
			entered, release := make(chan struct{}), make(chan struct{})
			var hold atomic.Bool
			hold.Store(true)
			r.floor.beforeLower = func(string) {
				if hold.CompareAndSwap(true, false) {
					close(entered)
					<-release
				}
			}
			if tc.next != "sess-1" {
				r.setSession(tc.next)
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				tc.end(r)
			}()
			<-entered
			r.join(t, "agent-1", "ABC-12")
			r.deliver(t, "agent-1", "ghost-wispr floor\nThe release is ready.")
			if !r.floor.handUp("agent-1") {
				t.Fatal("control: the new membership's hand is not up")
			}
			close(release)
			<-done
			if !r.floor.handUp("agent-1") {
				t.Fatalf("the earlier membership's end took down the new membership's hand: %q", r.floor.events)
			}
			if got := r.roster.Joined(); len(got) != 1 || got[0].SessionID != tc.next {
				t.Fatalf("roster %+v, want agent-1 in %s: the earlier membership's end took the new one", got, tc.next)
			}
		})
	}
}
