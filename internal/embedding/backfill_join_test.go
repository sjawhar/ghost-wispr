package embedding

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// heldClient embeds after release is closed, for every call, and counts the
// calls that entered and the ones that returned.
type heldClient struct {
	release           chan struct{}
	entered, returned atomic.Int32
}

func (c *heldClient) Embed(_ context.Context, texts []string, _ TaskType) ([][]float32, error) {
	c.entered.Add(1)
	<-c.release
	defer c.returned.Add(1)
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{float32(i + 1)}
	}
	return out, nil
}

// TestBackfillMissingWaitsForItsWorkersWhenCancelled fails if BackfillMissing
// returns, its context cancelled while two workers index and a third session
// waits for a turn, before those two workers have ended: they write the
// store, which the app closes once the backfill has returned.
func TestBackfillMissingWaitsForItsWorkersWhenCancelled(t *testing.T) {
	store := newTestStore(t)
	for i := range 3 {
		id := fmt.Sprintf("session-%d", i)
		if err := store.CreateSession(id, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		if err := store.AppendSegment(id, transcribe.Segment{Text: "synthetic words of " + id, Timestamp: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		if err := store.EndSession(id, time.Now().UTC(), "", ""); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.Canonicalize(id); err != nil {
			t.Fatal(err)
		}
	}
	client := &heldClient{release: make(chan struct{})}
	indexer := NewIndexer(client, store, 500)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- indexer.BackfillMissing(ctx) }()
	for deadline := time.Now().Add(5 * time.Second); client.entered.Load() < 2; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("control: %d workers reached the embedding client, want 2", client.entered.Load())
		}
	}
	cancel()
	select {
	case err := <-done:
		t.Fatalf("BackfillMissing returned (%v) while %d of its workers were still indexing", err, 2-client.returned.Load())
	case <-time.After(200 * time.Millisecond):
	}
	close(client.release)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("BackfillMissing = %v, want %v", err, context.Canceled)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("BackfillMissing did not return once its workers could end")
	}
	if got := client.returned.Load(); got != 2 {
		t.Fatalf("%d workers had ended when BackfillMissing returned, want 2", got)
	}
}
