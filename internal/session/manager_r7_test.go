package session

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

type r7IndexingSpy struct{ called chan struct{} }

func (s r7IndexingSpy) IndexSessionAtGeneration(context.Context, string, string, int) error {
	s.called <- struct{}{}
	return nil
}

type r7GenerationBumpingSummarizer struct{ store *storeMock }

func (s r7GenerationBumpingSummarizer) Summarize(_ context.Context, sessionID, _ string) (string, string, string, string, error) {
	s.store.mu.Lock()
	s.store.canonicalizeGeneration[sessionID]++
	s.store.mu.Unlock()
	return "Title", "Summary", "default", "{}", nil
}

func TestR7GenerateSummaryReturnsAfterCanonicalizeError(t *testing.T) {
	store := newStoreMock()
	store.canonicalizeErr = errors.New("canonicalization failed")
	summarizerCalled := make(chan string, 1)
	manager := NewManager(store, nil, summarizerMock{called: summarizerCalled}, nil, NewDetector(time.Hour), 0)

	done := make(chan struct{})
	go func() {
		manager.generateSummary(context.Background(), "session", time.Time{})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("generateSummary did not return after Canonicalize failed")
	}
	select {
	case sessionID := <-summarizerCalled:
		t.Fatalf("summarizer called for %q after Canonicalize failed", sessionID)
	default:
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.status["session"] != storage.SummaryFailed || !strings.Contains(store.errorMessage["session"], "canonicalization failed") {
		t.Fatalf("summary after Canonicalize error = status %q, error %q; want failed status with the canonicalization error", store.status["session"], store.errorMessage["session"])
	}
}

func TestR7NoSummarizerSyncsAfterCanonicalizeError(t *testing.T) {
	store := newStoreMock()
	store.canonicalizeErr = errors.New("canonicalization failed")
	synced := make(chan string, 1)
	indexer := r7IndexingSpy{called: make(chan struct{}, 1)}
	manager := NewManager(store, nil, nil, nil, NewDetector(time.Hour), 0)
	manager.SetSyncer(syncerMock{called: synced})
	manager.SetIndexer(indexer)

	manager.generateSummary(context.Background(), "session", time.Time{})
	select {
	case sessionID := <-synced:
		if sessionID != "session" {
			t.Fatalf("sync session = %q, want session", sessionID)
		}
	case <-time.After(time.Second):
		t.Fatal("Drive sync did not start after Canonicalize failed without a summarizer")
	}
	select {
	case <-indexer.called:
		t.Fatal("indexing started after Canonicalize failed")
	default:
	}
}

func TestR7SummaryCarriesCanonicalizeGenerationToItsWrite(t *testing.T) {
	store := newStoreMock()
	if err := store.CreateSession("session", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSegment("session", transcribe.Segment{Text: "summary input"}); err != nil {
		t.Fatal(err)
	}
	store.canonicalizeGeneration["session"] = 7
	manager := NewManager(store, nil, r7GenerationBumpingSummarizer{store: store}, nil, NewDetector(time.Hour), 0)

	manager.generateSummary(context.Background(), "session", time.Time{})

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.summaryWriteGenerations) != 1 || store.summaryWriteGenerations[0] != 7 {
		t.Fatalf("summary write generations = %v, want [7]", store.summaryWriteGenerations)
	}
	if store.status["session"] != storage.SummaryFailed || store.summary["session"] != "" {
		t.Fatalf("summary after generation changed = status %q, text %q; want failed without storing stale text", store.status["session"], store.summary["session"])
	}
}
