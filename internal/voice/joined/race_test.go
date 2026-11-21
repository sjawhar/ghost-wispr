package joined

import (
	"context"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/envoy"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// The tests here hold one roster operation inside a slow read while a
// meeting or a membership ends, then let it finish: what it decided from its
// read must not outlive what ended meanwhile.

// TestAJoinOrSegmentInFlightCannotReviveAnEndedMeeting fails if a join and a
// final segment that both began before their meeting ended, and was deleted
// through the store, finish after it: a join that commits the ended meeting
// as a membership, or a segment that sends its pointer. (ReviewPR122b's
// TestReviewCPendingJoinAndSegmentCannotReviveDeletedMeeting.)
func TestAJoinOrSegmentInFlightCannotReviveAnEndedMeeting(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "unit.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.CreateSession("sess-1", meetingStart); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSegment("sess-1", transcribe.Segment{Text: "ABC-12 is late", StartTime: 297, EndTime: 300, Timestamp: meetingStart.Add(300 * time.Second)}); err != nil {
		t.Fatal(err)
	}
	joinEntered, joinRelease := make(chan struct{}), make(chan struct{})
	segmentEntered, segmentRelease := make(chan struct{}), make(chan struct{})
	var holdSegment atomic.Bool
	r := newRig(t, func(cfg *Config) {
		now := cfg.Now
		cfg.Now = func() time.Time {
			if holdSegment.CompareAndSwap(true, false) {
				close(segmentEntered)
				<-segmentRelease
			}
			return now()
		}
		cfg.Started = func(id string) time.Time {
			session, err := store.GetSession(id)
			if err != nil {
				return time.Time{}
			}
			return session.StartedAt
		}
		cfg.Title = func(context.Context, string) (string, error) {
			close(joinEntered)
			<-joinRelease
			return titles["ABC-12"], nil
		}
	})
	joinDone := make(chan struct{})
	go func() {
		defer close(joinDone)
		_, _ = r.roster.Deliver(context.Background(), &envoy.Message{EventID: "join-in-flight", SourceSession: "agent-1", Text: "ghost-wispr join ABC-12", IssuedAt: r.clock.Now()})
	}()
	<-joinEntered
	holdSegment.Store(true)
	segmentDone := make(chan struct{})
	go func() {
		defer close(segmentDone)
		r.roster.OnSegment(&voice.Segment{SessionID: "sess-1", Text: "ABC-12 is late", From: meetingStart.Add(297 * time.Second), To: meetingStart.Add(300 * time.Second)})
	}()
	<-segmentEntered

	if err := store.EndSession("sess-1", r.clock.Now(), "", ""); err != nil {
		t.Fatal(err)
	}
	r.setSession("")
	r.roster.SessionEnded("sess-1")
	if _, err := store.DeleteSession("sess-1", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	r.roster.SessionsDeleted([]string{"sess-1"})
	if _, err := store.GetSession("sess-1"); err == nil {
		t.Fatal("control: the delete left the recording session")
	}

	close(joinRelease)
	<-joinDone
	close(segmentRelease)
	<-segmentDone
	r.roster.wg.Wait()
	r.roster.mu.Lock()
	members := len(r.roster.members)
	r.roster.mu.Unlock()
	if members != 0 {
		t.Fatalf("the join in flight joined the deleted meeting: %d members", members)
	}
	if got := r.pointers("agent-1"); len(got) != 0 {
		t.Fatalf("a pointer to the deleted meeting was sent: %q", got[0].Message)
	}
	if got := r.notes.all(); slices.Contains(got, "agent_joined:sess-1") {
		t.Fatalf("/ws events %q: the ended meeting gained an agent", got)
	}
}

// TestAFloorRequestInFlightCannotOutliveItsAgent fails if a floor request
// that read its agent's membership before the agent left raises a hand that
// stays up once both are done.
func TestAFloorRequestInFlightCannotOutliveItsAgent(t *testing.T) {
	r := newRig(t, nil)
	r.join(t, "agent-1", "ABC-12")
	raiseEntered, raiseRelease := make(chan struct{}), make(chan struct{})
	var hold atomic.Bool
	hold.Store(true)
	r.floor.beforeRaise = func(*voice.Hand) {
		if hold.CompareAndSwap(true, false) {
			close(raiseEntered)
			<-raiseRelease
		}
	}
	floorDone := make(chan struct{})
	go func() {
		defer close(floorDone)
		_, _ = r.roster.Deliver(context.Background(), &envoy.Message{EventID: "floor-in-flight", SourceSession: "agent-1", Text: "ghost-wispr floor\nThe release moved.", IssuedAt: r.clock.Now()})
	}()
	<-raiseEntered
	leaveDone := make(chan struct{})
	go func() {
		defer close(leaveDone)
		_, _ = r.roster.Deliver(context.Background(), &envoy.Message{EventID: "leave", SourceSession: "agent-1", Text: "ghost-wispr leave", IssuedAt: r.clock.Now()})
	}()
	select { // the leave may wait for the floor request; give it the time to run first if it does not
	case <-leaveDone:
	case <-time.After(100 * time.Millisecond):
	}
	close(raiseRelease)
	<-floorDone
	<-leaveDone
	if r.floor.handUp("agent-1") {
		t.Fatalf("a hand is up for an agent that left: %q", r.floor.events)
	}
}

// TestASweepInFlightCannotMarkANewMembership fails if a sweep's read that
// an agent was not live, made before it left and joined again, starts the new
// membership's 10 minutes not live.
func TestASweepInFlightCannotMarkANewMembership(t *testing.T) {
	r := newRig(t, nil)
	r.join(t, "agent-1", "ABC-12")
	r.agents.setState("agent-1", envoy.SessionInfo{SessionID: "agent-1", State: envoy.StateUnresponsive})
	lookupEntered, lookupRelease := make(chan struct{}), make(chan struct{})
	var hold atomic.Bool
	hold.Store(true)
	r.agents.beforeLookup = func(string) {
		if hold.CompareAndSwap(true, false) {
			close(lookupEntered)
			<-lookupRelease
		}
	}
	sweepDone := make(chan struct{})
	go func() {
		defer close(sweepDone)
		r.roster.Sweep(context.Background())
	}()
	<-lookupEntered
	r.deliver(t, "agent-1", "ghost-wispr leave")
	r.join(t, "agent-1", "ABC-12")
	close(lookupRelease)
	<-sweepDone

	r.clock.advance(10 * time.Minute)
	r.roster.Sweep(context.Background())
	if len(r.roster.Joined()) != 1 {
		t.Fatal("the new membership left at its first sweep: a read from before it counted toward its 10 minutes")
	}
}

// TestAMeetingsEndCannotTakeANewMembership fails if a meeting's end, which
// lists its agents before it makes them leave, takes an agent that joined
// the next meeting in between.
func TestAMeetingsEndCannotTakeANewMembership(t *testing.T) {
	r := newRig(t, nil)
	r.join(t, "agent-1", "ABC-12")
	r.join(t, "agent-2", "XYZ-9")
	var first atomic.Value
	lowerEntered, lowerRelease := make(chan struct{}), make(chan struct{})
	r.floor.beforeLower = func(agent string) {
		if first.CompareAndSwap(nil, agent) {
			close(lowerEntered)
			<-lowerRelease
		}
	}
	endDone := make(chan struct{})
	r.setSession("sess-2")
	go func() {
		defer close(endDone)
		r.roster.SessionEnded("sess-1")
	}()
	<-lowerEntered
	other := map[string]string{"agent-1": "agent-2", "agent-2": "agent-1"}[first.Load().(string)]
	r.join(t, other, map[string]string{"agent-1": "ABC-12", "agent-2": "XYZ-9"}[other])
	close(lowerRelease)
	<-endDone
	got := r.roster.Joined()
	if len(got) != 1 || got[0].Agent != other || got[0].SessionID != "sess-2" {
		t.Fatalf("roster %+v, want %s in sess-2: the end of sess-1 took its new membership", got, other)
	}
}
