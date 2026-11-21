package envoy

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// closeRecorder records each resume point saved after Close returned: the
// unit closes its store then, and a save is a write to it.
type closeRecorder struct {
	closed atomic.Bool
	mu     sync.Mutex
	late   []ResumePoint
}

func (c *closeRecorder) save(point ResumePoint) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed.Load() {
		c.late = append(c.late, point)
	}
	return nil
}

// closeWhileHeld closes session while held work runs, and fails if the work
// saved a resume point after Close returned. ended is closed once the held
// work has ended; when Close waits for it, ended is closed before Close
// returns, and the test fails if it is not.
func closeWhileHeld(t *testing.T, session *Session, saves *closeRecorder, ended <-chan struct{}, closeWaits bool) {
	t.Helper()
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		_ = session.Close(context.Background())
		saves.closed.Store(true)
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not end: it does not cut off the work it waits for")
	}
	if closeWaits {
		select {
		case <-ended:
		default:
			t.Error("Close returned while the held work still ran")
		}
	}
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the held work did not end")
	}
	time.Sleep(100 * time.Millisecond) // a save the work left would land now
	saves.mu.Lock()
	defer saves.mu.Unlock()
	if len(saves.late) != 0 {
		t.Errorf("resume points saved after Close returned: %+v", saves.late)
	}
}

// TestCloseWaitsForTheRelayFrameBeingDelivered fails if Close returns while a
// relay frame's delivery still runs (NATS's Close does not wait for a running
// callback), so its resume point is saved after the unit closed its store; or
// if a frame that comes after Close is delivered.
func TestCloseWaitsForTheRelayFrameBeingDelivered(t *testing.T) {
	_, server := newFakeListener(t)
	saves := &closeRecorder{}
	delivering, ended := make(chan struct{}), make(chan struct{})
	var delivered atomic.Int32
	session := testSession(t, server, func(ctx context.Context, _ *Message) error {
		delivered.Add(1)
		close(delivering)
		// Closed as the delivery returns, inside relay: relay's own return
		// lets Close return (stopped), so a close after it could come after
		// Close's.
		defer close(ended)
		select { // a delivery that takes its time
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
			return nil
		}
	}, nil)
	session.cfg.SaveResume = saves.save
	frame := nats.Header{}
	frame.Set("Envoy-Stream-Id", "stream-A")
	frame.Set("Envoy-Stream-Seq", "7")
	frame.Set("Envoy-Subject", DeliverSubjectPrefix+"pi-session-1")
	var answers []string
	go session.relay(frame, []byte(`{"event_id":"evt-7","source_session":"agent-7","payload_summary":"merged"}`), func(body string) { answers = append(answers, body) })
	select {
	case <-delivering:
	case <-time.After(5 * time.Second):
		t.Fatal("control: the frame was not delivered")
	}
	closeWhileHeld(t, session, saves, ended, true)
	if last := answers[len(answers)-1]; last != "nak" {
		t.Errorf("the frame Close cut off was answered %q, want nak, so it comes again", answers)
	}

	session.relay(frame, []byte(`{"event_id":"evt-8"}`), func(string) { t.Error("a frame after Close was answered") })
	if n := delivered.Load(); n != 1 {
		t.Errorf("%d deliveries, want only the frame before Close", n)
	}
}

// TestCloseStopsWaitingAtItsDeadline fails if Close waits past its context's
// end for a relay frame's delivery that does not see the cancel, as a store
// write waiting for the store's connection does not: the unit's shutdown
// would hang there and never close its store. The control is
// TestCloseWaitsForTheRelayFrameBeingDelivered, where the delivery ends and
// Close waits for it.
func TestCloseStopsWaitingAtItsDeadline(t *testing.T) {
	_, server := newFakeListener(t)
	delivering, release, ended := make(chan struct{}), make(chan struct{}), make(chan struct{})
	session := testSession(t, server, func(context.Context, *Message) error {
		close(delivering)
		<-release // it does not see the cancel
		return nil
	}, nil)
	defer close(release)
	frame := nats.Header{}
	frame.Set("Envoy-Stream-Id", "stream-A")
	frame.Set("Envoy-Stream-Seq", "7")
	frame.Set("Envoy-Subject", DeliverSubjectPrefix+"pi-session-1")
	go func() {
		defer close(ended)
		session.relay(frame, []byte(`{"event_id":"evt-7"}`), func(string) {})
	}()
	select {
	case <-delivering:
	case <-time.After(5 * time.Second):
		t.Fatal("control: the frame was not delivered")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	closed := make(chan error, 1)
	go func() { closed <- session.Close(ctx) }()
	select {
	case err := <-closed:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Close at its deadline = %v, want its context's cause", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close waited past its deadline for a delivery that does not see the cancel")
	}
	select {
	case <-ended:
		t.Error("control: the held delivery ended before its release")
	default:
	}
}

// TestCloseWaitsForAReconnectsRegistration fails if Close returns while the
// registration a NATS reconnect started still runs, so the resume point its
// answer names is saved after the unit closed its store, or if a reconnect
// after Close registers.
func TestCloseWaitsForAReconnectsRegistration(t *testing.T) {
	fake, server := newFakeListener(t)
	registering, ended := make(chan struct{}), make(chan struct{})
	var registrations atomic.Int32
	fake.mu.Lock()
	fake.answers["/v1/interests/subscribe"] = func(w http.ResponseWriter, r *http.Request) {
		if registrations.Add(1) == 1 {
			close(registering)
			defer close(ended)
		}
		select { // a listener slow to answer
		case <-r.Context().Done():
			return
		case <-time.After(300 * time.Millisecond):
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"session_id":"pi-session-1","delivery":{"mode":"consumer","stream_id":"stream-B","consumer_start":9}}`))
	}
	fake.mu.Unlock()
	saves := &closeRecorder{}
	session := testSession(t, server, nil, nil)
	session.cfg.SaveResume = saves.save
	session.reconnected()
	select {
	case <-registering:
	case <-time.After(5 * time.Second):
		t.Fatal("control: the reconnect did not register")
	}
	// The listener's handler may see the cancelled request only after
	// Close returned, so only the late save is checked.
	closeWhileHeld(t, session, saves, ended, false)

	session.reconnected()
	time.Sleep(50 * time.Millisecond)
	if n := registrations.Load(); n != 1 {
		t.Errorf("%d registrations, want only the reconnect's before Close", n)
	}
}
