package embedding

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/providererr"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// fakeClock is a clock that a Sleep moves forward at once, so a test waits
// out a gateway's minute of 429s in no time.
type fakeClock struct {
	mu    sync.Mutex
	now   time.Time
	slept time.Duration
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	c.now, c.slept = c.now.Add(d), c.slept+d
	c.mu.Unlock()
	return ctx.Err()
}

// limitedGateway is a model gateway's embeddings route that answers 429 to
// each call refuse names, by the call's input and how many calls that input
// has had.
type limitedGateway struct {
	refuse func(input string, call int) bool

	mu   sync.Mutex
	sent map[string]int
}

func (g *limitedGateway) serve(t *testing.T) *httptest.Server {
	t.Helper()
	g.sent = map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Input string `json:"input"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("a call that is not one string input: %s", raw)
		}
		g.mu.Lock()
		g.sent[body.Input]++
		call := g.sent[body.Input]
		g.mu.Unlock()
		if g.refuse(body.Input, call) {
			rateLimit(w)
			return
		}
		openAIEmbedding(w, []float32{float32(len(body.Input))})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (g *limitedGateway) calls(input string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.sent[input]
}

func (g *limitedGateway) total() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	sum := 0
	for _, n := range g.sent {
		sum += n
	}
	return sum
}

// gatewayIndexer is an indexer of store through g, of chunks of chunkSize
// words, whose retries wait on clk.
func gatewayIndexer(t *testing.T, g *limitedGateway, store *storage.SQLiteStore, chunkSize int, clk *fakeClock) *Indexer {
	t.Helper()
	client, err := NewClient(gatewayProvider(t, g.serve(t).URL+"/v1"), "gemini-embedding-2")
	if err != nil {
		t.Fatal(err)
	}
	idx := NewIndexer(client, store, chunkSize)
	idx.retryPolicy.Sleep, idx.retryPolicy.Now = clk.Sleep, clk.Now
	return idx
}

func storedChunks(t *testing.T, store *storage.SQLiteStore, sessionID string) []int {
	t.Helper()
	stored, err := store.GetEmbeddings(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	indexes := make([]int, 0, len(stored))
	for _, e := range stored {
		indexes = append(indexes, e.ChunkIndex)
	}
	slices.Sort(indexes)
	return indexes
}

// TestARateLimitLongerThanMaxRetriesIsWaitedOut fails if the indexer gives
// up a chunk the gateway refuses 429 six times in a row (more tries than
// retry.DefaultMaxRetries, well within a minute's limit), or sends again a
// chunk already embedded while it waits.
func TestARateLimitLongerThanMaxRetriesIsWaitedOut(t *testing.T) {
	transcript := makeTranscript(700)
	chunks := SplitIntoChunks(transcript, 500)
	g := &limitedGateway{refuse: func(input string, call int) bool { return input == chunks[1] && call <= 6 }}
	store := newTestStore(t)
	if err := store.CreateSession("s", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	generation, err := store.DeletionGeneration("s")
	if err != nil {
		t.Fatal(err)
	}
	clk := &fakeClock{now: time.Unix(1_800_000_000, 0)}
	idx := gatewayIndexer(t, g, store, 500, clk)

	if err := idx.IndexSessionAtGeneration(context.Background(), "s", transcript, generation); err != nil {
		t.Fatalf("IndexSessionAtGeneration through six 429s: %v", err)
	}
	if got := storedChunks(t, store, "s"); !slices.Equal(got, []int{0, 1}) {
		t.Fatalf("chunks %v stored, want both", got)
	}
	if first, second := g.calls(chunks[0]), g.calls(chunks[1]); first != 1 || second != 7 {
		t.Fatalf("chunk 0 sent %d times and chunk 1 %d; want 1 and 7 (six 429s, then the vector)", first, second)
	}
	if clk.slept > 90*time.Second {
		t.Fatalf("waited %s, past the 90s budget", clk.slept)
	}
}

// TestARateLimitPastTheBudgetKeepsTheChunksDone fails if a chunk the gateway
// refuses 429 past the budget does not fail the index, if the chunks embedded
// before it are not kept, or if the next BackfillMissing or re-index does not
// finish the session without sending those chunks again. BackfillMissing
// then sends nothing more for the finished session.
func TestARateLimitPastTheBudgetKeepsTheChunksDone(t *testing.T) {
	for _, finish := range []string{"BackfillMissing", "re-index"} {
		t.Run(finish, func(t *testing.T) {
			store := newTestStore(t)
			const id = "limited"
			if err := store.CreateSession(id, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			if err := store.EndSession(id, time.Now().UTC(), "", ""); err != nil {
				t.Fatal(err)
			}
			if err := store.AppendSegment(id, transcribe.Segment{Text: makeTranscript(12), Timestamp: time.Now().UTC()}); err != nil {
				t.Fatal(err)
			}
			transcript, generation, err := store.Canonicalize(id)
			if err != nil {
				t.Fatal(err)
			}
			chunks := SplitIntoChunks(transcript, 5)
			if len(chunks) < 3 {
				t.Fatalf("%d chunks of %q; the test needs three or more", len(chunks), transcript)
			}
			var open atomic.Bool
			g := &limitedGateway{refuse: func(input string, _ int) bool { return input == chunks[1] && !open.Load() }}
			clk := &fakeClock{now: time.Unix(1_800_000_000, 0)}
			idx := gatewayIndexer(t, g, store, 5, clk)

			err = idx.IndexSessionAtGeneration(context.Background(), id, transcript, generation)
			var answered *providererr.Error
			if !errors.As(err, &answered) || answered.Status != http.StatusTooManyRequests {
				t.Fatalf("IndexSessionAtGeneration past the budget = %v; want the 429", err)
			}
			if got := storedChunks(t, store, id); !slices.Equal(got, []int{0}) {
				t.Fatalf("chunks %v stored after the 429; want chunk 0, embedded before it", got)
			}
			if clk.slept != 90*time.Second {
				t.Fatalf("waited %s on chunk 1; want the 90s budget", clk.slept)
			}

			open.Store(true)
			if finish == "BackfillMissing" {
				err = idx.BackfillMissing(context.Background())
			} else {
				err = idx.IndexSessionAtGeneration(context.Background(), id, transcript, generation)
			}
			if err != nil {
				t.Fatalf("%s: %v", finish, err)
			}
			want := make([]int, len(chunks))
			for i := range want {
				want[i] = i
			}
			if got := storedChunks(t, store, id); !slices.Equal(got, want) {
				t.Fatalf("chunks %v stored after %s; want %v", got, finish, want)
			}
			if n := g.calls(chunks[0]); n != 1 {
				t.Fatalf("chunk 0 sent %d times; want once, kept from before the 429", n)
			}

			before := g.total()
			if err := idx.BackfillMissing(context.Background()); err != nil {
				t.Fatal(err)
			}
			if after := g.total(); after != before {
				t.Fatalf("BackfillMissing sent %d calls for a finished session", after-before)
			}
		})
	}
}
