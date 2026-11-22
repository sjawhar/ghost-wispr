package main

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/embedding"
	"github.com/sjawhar/ghost-wispr/internal/session"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/storeguard"
)

// stopTestManager is a session manager on store whose background work stops,
// at the test's end, before the store closes, as main's stop does.
func stopTestManager(t *testing.T, store *storage.SQLiteStore) *session.Manager {
	t.Helper()
	manager := session.NewManager(store, nil, nil, nil, session.NewDetector(time.Hour), 0)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := manager.StopBackground(ctx); err != nil {
			t.Errorf("the session manager's background work did not stop: %v", err)
		}
	})
	return manager
}

// TestTheStopWaitsForTheAPIsSummaryBacklog fails if the stop returns while
// an online summary of a backlog the API asked for (POST
// /api/sessions/backfill, or POST /api/sessions/batch-backfill gone online),
// wired as main wires it (backlogUntilStop), still runs: the store closes
// after the stop, under it. It also fails if the summary it cut off is
// recorded failed instead of left running for the next start's backlog.
func TestTheStopWaitsForTheAPIsSummaryBacklog(t *testing.T) {
	store := openResumeStore(t, filepath.Join(t.TempDir(), "backlog.db"))
	const id = "20261120090000"
	endedSessionWithWords(t, store, id, storage.SummaryFailed)
	manager := session.NewManager(store, nil, nil, nil, session.NewDetector(time.Hour), 0)
	summarizer := &heldSummarizer{entered: make(chan struct{}), stall: 100 * time.Millisecond}
	later := backlogUntilStop(manager, newOnlineSummaries(store, fixedSummarizerOf(summarizer), &recordingBroadcaster{}, func(string) {}))
	later([]string{id}) // as the route does, once it has answered
	stopDuringSummary(t, store, manager, summarizer, id, nil)
}

// heldEmbeddings is an embedding provider whose answer comes once the call's
// context has ended and stall has passed, as an answer to a call the stop cut
// off can: the index then stores it.
type heldEmbeddings struct {
	entered chan struct{}
	once    sync.Once
	stall   time.Duration
}

func (h *heldEmbeddings) Embed(ctx context.Context, texts []string, _ embedding.TaskType) ([][]float32, error) {
	h.once.Do(func() { close(h.entered) })
	<-ctx.Done()
	time.Sleep(h.stall)
	vectors := make([][]float32, len(texts))
	for i := range texts {
		vectors[i] = []float32{1, 0}
	}
	return vectors, nil
}

// TestTheStopWaitsForTheSearchBackfill fails if the stop returns while a
// search backfill (searchIndexer.requestBackfill: the start's, a settings
// save's, or one a good index asks for), wired as main wires it, still runs,
// or if it stores a vector once the stop has returned, when main closes the
// store (storeguard). The backfill runs on the app's context, as main runs
// it, which the stop does not end: only the session manager's stop cuts it
// off.
func TestTheStopWaitsForTheSearchBackfill(t *testing.T) {
	store := openResumeStore(t, filepath.Join(t.TempDir(), "search.db"))
	const id = "20261120090000"
	endedSessionWithWords(t, store, id, storage.SummaryCompleted)
	if _, _, err := store.Canonicalize(id); err != nil {
		t.Fatal(err)
	}
	manager := session.NewManager(store, nil, nil, nil, session.NewDetector(time.Hour), 0)
	appCtx, stopApp := newAppContext()
	t.Cleanup(stopApp)
	client := &heldEmbeddings{entered: make(chan struct{}), stall: 100 * time.Millisecond}
	indexer := newSearchIndexer(appCtx, manager, client, "gemini-embedding-2", "vertex/gemini-embedding-2", store, newWarningsRegistry())
	guard := storeguard.New(t, store)
	indexer.requestBackfill()
	select {
	case <-client.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("control: the backfill asked the provider for nothing")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	began := time.Now()
	recoverAtStart(store).stop(shutdownCtx, manager, lifecyclePublishWait)
	took := time.Since(began)
	guard.Close(t)
	if took < client.stall || took > time.Second {
		t.Fatalf("the stop took %v, want it to wait the %v its cut-off backfill takes, and no longer", took, client.stall)
	}
	time.Sleep(2 * client.stall) // a backfill the stop left would store now
	guard.Check(t)
}
