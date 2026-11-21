package session

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/stopping"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// countingSummarizer counts its calls and makes an empty summary.
type countingSummarizer struct{ calls atomic.Int32 }

func (s *countingSummarizer) Summarize(context.Context, string, string) (string, string, string, string, error) {
	s.calls.Add(1)
	return "", "", "", "", nil
}

// nopBatch is a batch transcriber that hears nothing.
type nopBatch struct{ calls atomic.Int32 }

func (b *nopBatch) Transcribe(context.Context, string) (transcribe.BatchResult, error) {
	b.calls.Add(1)
	return transcribe.BatchResult{}, errors.New("nothing heard")
}

// TestTheSessionEndedHookRunsAsWorkTheStopWaitsFor fails if StopBackground
// returns while the session-ended hook still runs (the Concierge's part of it
// writes the store, which closes after the stop), or if a session that ends
// after the stop runs the hook.
func TestTheSessionEndedHookRunsAsWorkTheStopWaitsFor(t *testing.T) {
	store := newStoreMock()
	manager := NewManager(store, nil, nil, nil, NewDetector(time.Hour), 0)
	entered, release := make(chan string, 2), make(chan struct{})
	manager.SetSessionEndedHook(func(sessionID string) {
		entered <- sessionID
		<-release
	})
	end := func() {
		t.Helper()
		if err := manager.ensureSessionStarted(time.Now().UTC()); err != nil {
			t.Fatalf("start session: %v", err)
		}
		if err := store.AppendSegment(manager.currentSession(), transcribe.Segment{Text: "one segment"}); err != nil {
			t.Fatal(err)
		}
		if err := manager.endCurrentSession(context.Background()); err != nil {
			t.Fatalf("end session: %v", err)
		}
	}
	end()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("control: the session-ended hook never ran")
	}
	stopped := make(chan error, 1)
	go func() { stopped <- manager.StopBackground(context.Background()) }()
	select {
	case err := <-stopped:
		t.Fatalf("StopBackground returned (%v) while the session-ended hook still ran", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("StopBackground did not return once the hook ended")
	}
	end()
	select {
	case id := <-entered:
		t.Fatalf("a session that ended after the stop (%s) ran the session-ended hook", id)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestRefineAndSummarizeSessionAreRefusedOnceStopped fails if RefineSession
// or SummarizeSession runs once StopBackground has run, whoever calls them:
// the merge hook, Retry refinement, Delete text and a kept recording's save
// call them, and the store closes after the stop.
func TestRefineAndSummarizeSessionAreRefusedOnceStopped(t *testing.T) {
	summarizer := &countingSummarizer{}
	manager := NewManager(newStoreMock(), nil, summarizer, nil, NewDetector(time.Hour), 0)
	batch := &nopBatch{}
	manager.SetBatchTranscriber(batch)
	if err := manager.StopBackground(context.Background()); err != nil {
		t.Fatal(err)
	}
	started := time.Now().UTC()
	if err := manager.RefineSession(context.Background(), "s", "/audio/s.mp3", started); !errors.Is(err, stopping.ErrStopped) {
		t.Errorf("RefineSession after the stop = %v, want %v", err, stopping.ErrStopped)
	}
	if err := manager.SummarizeSession(context.Background(), "s", started); !errors.Is(err, stopping.ErrStopped) {
		t.Errorf("SummarizeSession after the stop = %v, want %v", err, stopping.ErrStopped)
	}
	if batch.calls.Load() != 0 || summarizer.calls.Load() != 0 {
		t.Fatalf("after the stop, the refinement ran %d times and the summary %d times; want neither", batch.calls.Load(), summarizer.calls.Load())
	}
}
