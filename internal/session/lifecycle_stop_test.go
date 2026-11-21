package session

import (
	"context"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/envoy"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/storeguard"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// heldEndPublisher holds each session_ended send until its context ends or
// release closes, then marks the event delivered, as the Envoy publisher
// does: a send it holds writes the store when it ends.
type heldEndPublisher struct {
	store   *storage.SQLiteStore
	release chan struct{}
	sending chan struct{}
	once    sync.Once
	sends   atomic.Int32
}

func (*heldEndPublisher) PublishSessionStarted(context.Context, string) error { return nil }
func (*heldEndPublisher) PublishSummaryReady(context.Context, string) error   { return nil }

func (p *heldEndPublisher) PublishSessionEnded(ctx context.Context, sessionID string) error {
	p.sends.Add(1)
	if err := p.store.EnqueueEnvoyLifecycleEvent("session_ended", sessionID); err != nil {
		return err
	}
	p.once.Do(func() { close(p.sending) })
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.release:
	}
	return p.store.MarkEnvoyLifecycleEventDelivered("session_ended", sessionID)
}

// TestTheStopWaitsForASessionEndedSendItsLifecycleWaitMissed fails if a
// session_ended send that starts after the stop's lifecycle wait (a session
// that ends then) writes its delivery after StopBackground returned, when
// main closes the store (storeguard records it), or if a send starts once
// StopBackground has run.
func TestTheStopWaitsForASessionEndedSendItsLifecycleWaitMissed(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "lifecycle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	manager := NewManager(store, &recorderMock{}, nil, nil, NewDetector(time.Hour), 0)
	publisher := &heldEndPublisher{store: store, release: make(chan struct{}), sending: make(chan struct{})}
	manager.SetEventPublisher(publisher)
	ctx := context.Background()
	if _, err := manager.ManualStartSession(ctx, ""); err != nil {
		t.Fatal(err)
	}
	guard := storeguard.New(t, store)

	// The stop's lifecycle wait, as main runs it, finds no send under way.
	manager.StopSessionStartPublishes()
	waitCtx, cancelWait := context.WithTimeout(ctx, time.Second)
	defer cancelWait()
	if err := manager.WaitForLifecyclePublishes(waitCtx); err != nil {
		t.Fatal(err)
	}
	if err := manager.ForceEndSession(ctx); err != nil { // a session that ends after it
		t.Fatal(err)
	}
	select {
	case <-publisher.sending:
	case <-time.After(5 * time.Second):
		t.Fatal("control: the session's end sent no session_ended")
	}
	stopCtx, cancelStop := context.WithTimeout(ctx, 5*time.Second)
	defer cancelStop()
	if err := manager.StopBackground(stopCtx); err != nil {
		t.Fatalf("the stop did not wait for the send: %v", err)
	}
	guard.Close(t)
	close(publisher.release)
	time.Sleep(100 * time.Millisecond) // a send the stop left would write now
	guard.Check(t)

	before := publisher.sends.Load()
	manager.publishSessionEnded("after-the-stop")
	time.Sleep(50 * time.Millisecond)
	if n := publisher.sends.Load(); n != before {
		t.Errorf("%d session_ended sends started after StopBackground, want none", n-before)
	}
}

// heldCountStore holds the session end's segment count, which it reads after
// it has saved the end and cleared the session, until release closes.
type heldCountStore struct {
	*storage.SQLiteStore
	entered, release chan struct{}
}

func (s *heldCountStore) CountSegments(id string) (int, error) {
	close(s.entered)
	<-s.release
	return s.SQLiteStore.CountSegments(id)
}

// countingTransport answers each Envoy send as delivered, with no network.
type countingTransport struct{ sends atomic.Int32 }

func (c *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c.sends.Add(1)
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"event_id":"local"}`)), Request: req}, nil
}

// TestASessionEndTheStopOutlastsLeavesItsEventQueued fails if a session end
// the stop admitted, which completes after the stop's lifecycle wait gave up
// and StopBackground refused new sends, leaves its session_ended event
// neither sent nor queued for the next start's sweep, or if that sweep
// (envoy.Publisher.PublishPendingLifecycleEvents) cannot send the queued
// event. The control is the end that completes before the stop, whose send
// delivers it. From review 6005610488 of #138.
func TestASessionEndTheStopOutlastsLeavesItsEventQueued(t *testing.T) {
	for _, outlasts := range []bool{false, true} {
		name := map[bool]string{false: "control: the end completes before the stop", true: "the end completes after the lifecycle wait gave up"}[outlasts]
		t.Run(name, func(t *testing.T) {
			db, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "lifecycle.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			store := &heldCountStore{SQLiteStore: db, entered: make(chan struct{}), release: make(chan struct{})}
			manager := NewManager(store, &recorderMock{}, nil, nil, NewDetector(time.Hour), 0)
			manager.SetMinSessionSegments(1)
			id, err := manager.ManualStartSession(context.Background(), "")
			if err != nil {
				t.Fatal(err)
			}
			if err := db.AppendSegment(id, transcribe.Segment{Text: "words kept"}); err != nil {
				t.Fatal(err)
			}
			transport := &countingTransport{}
			publisher, err := envoy.NewPublisher("http://envoy.invalid", db, envoy.WithHTTPClient(&http.Client{Transport: transport}))
			if err != nil {
				t.Fatal(err)
			}
			manager.SetEventPublisher(publisher)
			ended := make(chan error, 1)
			manager.Go(func(ctx context.Context) { ended <- manager.ForceEndSession(ctx) })
			select {
			case <-store.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("control: the end did not reach its segment count")
			}
			stopped := make(chan error, 1)
			if outlasts {
				waitCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
				defer cancel()
				if err := manager.WaitForLifecyclePublishes(waitCtx); !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("control: the lifecycle wait = %v, want its deadline while the end is held", err)
				}
				go func() { stopped <- manager.StopBackground(context.Background()) }()
				<-manager.background.Done()
			}
			close(store.release)
			if err := <-ended; err != nil {
				t.Fatal(err)
			}
			if outlasts {
				if err := <-stopped; err != nil {
					t.Fatal(err)
				}
			} else {
				if err := manager.WaitForLifecyclePublishes(context.Background()); err != nil {
					t.Fatal(err)
				}
				if err := manager.StopBackground(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			pending, err := db.PendingEnvoyLifecycleEvents()
			if err != nil {
				t.Fatal(err)
			}
			sent := transport.sends.Load()
			if outlasts && (sent != 0 || len(pending) != 1) {
				t.Fatalf("after the stop: %d sends and %d queued events, want the stop's refused send queued (0 and 1)", sent, len(pending))
			}
			if !outlasts && (sent != 1 || len(pending) != 0) {
				t.Fatalf("control: %d sends and %d queued events, want it delivered (1 and 0)", sent, len(pending))
			}
			if err := publisher.PublishPendingLifecycleEvents(context.Background()); err != nil { // the next start's sweep
				t.Fatal(err)
			}
			if pending, err = db.PendingEnvoyLifecycleEvents(); err != nil || len(pending) != 0 || transport.sends.Load() != 1 {
				t.Fatalf("after the sweep: %d sends, queued %v (%v), want the event delivered once", transport.sends.Load(), pending, err)
			}
		})
	}
}
