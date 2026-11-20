package main

import (
	"context"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/logtest"
	"github.com/sjawhar/ghost-wispr/internal/session"
)

// heldEndRecorder is a recorder whose first EndSession waits until release
// closes, as a recorder saving a long recording does.
type heldEndRecorder struct {
	held, release chan struct{}
	once          sync.Once
}

func (*heldEndRecorder) StartSession(string) error { return nil }
func (r *heldEndRecorder) EndSession(string) (string, error) {
	r.once.Do(func() { close(r.held); <-r.release })
	return "", nil
}
func (*heldEndRecorder) DiscardUnsaved() string            { return "" }
func (*heldEndRecorder) HoldPreroll(time.Duration)         {}
func (*heldEndRecorder) ReleasePreroll()                   {}
func (*heldEndRecorder) SetLookback(time.Duration)         {}
func (*heldEndRecorder) Pending() time.Duration            { return 0 }
func (*heldEndRecorder) SinceClose() (time.Duration, bool) { return 0, true }

// TestTheStopsSessionEndStaysInsideTheShutdownDeadline fails if the stop's
// end of the session (endSessionAtShutdown, as main calls it) waits past the
// shutdown's deadline. A pause's end held by the recorder takes the whole
// deadline from the stop's own end; the wait for the session's lifecycle
// events must then give up at that deadline, not start a time limit of its
// own: it did, from context.Background, and the stop ran on after its
// deadline for that limit's length. The real session manager and store; the
// deadlines are main's 30 s and 5 s, scaled to 50 ms and 150 ms.
func TestTheStopsSessionEndStaysInsideTheShutdownDeadline(t *testing.T) {
	store := openResumeStore(t, filepath.Join(t.TempDir(), "stop.db"))
	recorder := &heldEndRecorder{held: make(chan struct{}), release: make(chan struct{})}
	manager := session.NewManager(store, recorder, nil, nil, session.NewDetector(time.Hour), 0)
	if _, err := manager.ManualStartSession(t.Context(), ""); err != nil {
		t.Fatalf("start session: %v", err)
	}
	pauseEnded := make(chan error, 1)
	go func() { pauseEnded <- manager.ForceEndSession(context.Background()) }() // the pause's end
	defer func() {
		close(recorder.release)
		if err := <-pauseEnded; err != nil {
			t.Errorf("the pause's end: %v", err)
		}
	}()
	select {
	case <-recorder.held:
	case <-time.After(2 * time.Second):
		t.Fatal("the pause's end never reached the recorder")
	}

	const shutdownDeadline, lifecycleWait = 50 * time.Millisecond, 150 * time.Millisecond
	shutdownCtx, cancel := context.WithTimeout(t.Context(), shutdownDeadline)
	defer cancel()
	logs := logtest.Capture(t)
	start := time.Now()
	endSessionAtShutdown(shutdownCtx, manager, lifecycleWait)
	took := time.Since(start)
	var warned []string
	for _, record := range logs.Records() {
		if record.Level == "WARN" {
			warned = append(warned, record.Message)
		}
	}
	t.Logf("the stop's session end took %v; warned %q", took, warned)
	if took > shutdownDeadline+50*time.Millisecond {
		t.Fatalf("the stop's session end took %v, past its %v shutdown deadline", took, shutdownDeadline)
	}
	if want := []string{"force end session failed", "lifecycle event publishing did not finish before shutdown"}; !slices.Equal(warned, want) {
		t.Fatalf("warned %q, want the end's and the lifecycle wait's deadline warnings, %q", warned, want)
	}
}

// TestTheStopsSessionEndWaitsForTheEventsWithinItsLimit fails if the stop's
// end of the session gives up on its lifecycle events before lifecycleWait
// with time left on the shutdown deadline, or waits past it.
func TestTheStopsSessionEndWaitsForTheEventsWithinItsLimit(t *testing.T) {
	manager := &stoppingManager{}
	const lifecycleWait = 80 * time.Millisecond
	start := time.Now()
	endSessionAtShutdown(t.Context(), manager, lifecycleWait)
	took := time.Since(start)
	if took < lifecycleWait || took > lifecycleWait+100*time.Millisecond {
		t.Fatalf("the stop waited %v for its lifecycle events, want about %v", took, lifecycleWait)
	}
}

// stoppingManager ends at once, and its lifecycle events never finish.
type stoppingManager struct{}

func (*stoppingManager) ForceEndSession(context.Context) error { return nil }
func (*stoppingManager) WaitForLifecyclePublishes(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}
