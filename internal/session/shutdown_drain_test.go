package session

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recordingEndPublisher counts the session_ended sends that finished.
type recordingEndPublisher struct {
	ended atomic.Int32
}

func (*recordingEndPublisher) PublishSessionStarted(context.Context, string) error { return nil }
func (*recordingEndPublisher) PublishSummaryReady(context.Context, string) error   { return nil }
func (p *recordingEndPublisher) PublishSessionEnded(context.Context, string) error {
	p.ended.Add(1)
	return nil
}

// TestShutdownWaitsForTheSessionEndAPauseStarted fails if a shutdown can stop
// waiting before a session end already under way has sent its session_ended.
// On the office Pi on 2026-11-20 a pause ended the session while the service
// stopped: the shutdown's ForceEndSession found no session left (the pause's
// end had cleared it) and WaitForLifecyclePublishes found no send yet (the
// pause's end had not reached it), so the shutdown went on and closed the
// database, and the send that came after could not mark itself delivered:
// "mark envoy lifecycle event session_ended … delivered: sql: database is
// closed", logged at every such stop, and the event sent again at the next
// start. Here the pause's end is held after the session is cleared and before
// its send, as the hub's broadcast held it on the Pi.
func TestShutdownWaitsForTheSessionEndAPauseStarted(t *testing.T) {
	hub := &hubMock{}
	manager := NewManager(newStoreMock(), &recorderMock{}, nil, hub, NewDetector(time.Hour), 0)
	publisher := &recordingEndPublisher{}
	manager.SetEventPublisher(publisher)
	ctx := context.Background()
	if _, err := manager.ManualStartSession(ctx, ""); err != nil {
		t.Fatalf("start session: %v", err)
	}

	held, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	hub.endedHook = func(string) {
		once.Do(func() { close(held); <-release })
	}
	pauseEnded := make(chan error, 1)
	go func() { pauseEnded <- manager.ForceEndSession(ctx) }() // the pause's end
	select {
	case <-held:
	case <-time.After(2 * time.Second):
		t.Fatal("the pause's end never reached the hub")
	}

	// The shutdown, as main runs it: end the session, then wait for the sends.
	if err := manager.ForceEndSession(ctx); !errors.Is(err, ErrNoActiveSession) {
		t.Fatalf("the shutdown's ForceEndSession = %v, want ErrNoActiveSession: the pause's end has the session", err)
	}
	waited := make(chan error, 1)
	go func() {
		waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		waited <- manager.WaitForLifecyclePublishes(waitCtx)
	}()
	select {
	case err := <-waited:
		close(release)
		t.Fatalf("WaitForLifecyclePublishes returned (%v) while the pause's end had not yet sent its session_ended (%d sent): the shutdown would close the database under that send", err, publisher.ended.Load())
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-waited:
		if err != nil {
			t.Fatalf("WaitForLifecyclePublishes = %v, want nil once the end has sent", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WaitForLifecyclePublishes did not return after the pause's end finished")
	}
	if got := publisher.ended.Load(); got != 1 {
		t.Fatalf("session_ended was sent %d times by the time the shutdown stopped waiting, want 1", got)
	}
	if err := <-pauseEnded; err != nil {
		t.Fatalf("the pause's end: %v", err)
	}
}

// TestShutdownWaitForAnEndInFlightHonoursItsBound fails if a shutdown waits
// without a bound for a session end that does not finish.
func TestShutdownWaitForAnEndInFlightHonoursItsBound(t *testing.T) {
	hub := &hubMock{}
	manager := NewManager(newStoreMock(), &recorderMock{}, nil, hub, NewDetector(time.Hour), 0)
	manager.SetEventPublisher(&recordingEndPublisher{})
	ctx := context.Background()
	if _, err := manager.ManualStartSession(ctx, ""); err != nil {
		t.Fatalf("start session: %v", err)
	}
	held, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	var once sync.Once
	hub.endedHook = func(string) {
		once.Do(func() { close(held); <-release })
	}
	go func() { _ = manager.ForceEndSession(ctx) }()
	select {
	case <-held:
	case <-time.After(2 * time.Second):
		t.Fatal("the end never reached the hub")
	}
	waitCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := manager.WaitForLifecyclePublishes(waitCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitForLifecyclePublishes = %v, want its deadline", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("WaitForLifecyclePublishes took %v past a 50 ms deadline", took)
	}
}
