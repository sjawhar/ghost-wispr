package session

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/storeguard"
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
