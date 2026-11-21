package envoy

import (
	"context"
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
	go func() {
		defer close(ended)
		session.relay(frame, []byte(`{"event_id":"evt-7","source_session":"agent-7","payload_summary":"merged"}`), func(body string) { answers = append(answers, body) })
	}()
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
