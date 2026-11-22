package embedding

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// countingStore is a SQLite store that counts its reads of stored
// embeddings: of every session's (GetAllEmbeddings) and of one session's
// (GetEmbeddings).
type countingStore struct {
	*storage.SQLiteStore
	all, session atomic.Int32
}

func (s *countingStore) GetAllEmbeddings() ([]StoredEmbedding, error) {
	s.all.Add(1)
	return s.SQLiteStore.GetAllEmbeddings()
}

func (s *countingStore) GetEmbeddings(sessionID string) ([]StoredEmbedding, error) {
	s.session.Add(1)
	return s.SQLiteStore.GetEmbeddings(sessionID)
}

// countingClient answers each text with a vector and counts the texts it is
// sent, safe for BackfillMissing's indexes at once.
type countingClient struct{ texts atomic.Int32 }

func (c *countingClient) Embed(_ context.Context, texts []string, _ TaskType) ([][]float32, error) {
	c.texts.Add(int32(len(texts)))
	vectors := make([][]float32, len(texts))
	for i := range texts {
		vectors[i] = []float32{1, float32(i)}
	}
	return vectors, nil
}

// endedSession stores the ended session id, whose canonical transcript is
// text.
func endedSession(t *testing.T, store *storage.SQLiteStore, id, text string) {
	t.Helper()
	now := time.Now().UTC()
	if err := store.CreateSession(id, now); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession(id, now, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSegment(id, transcribe.Segment{Text: text, Timestamp: now}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Canonicalize(id); err != nil {
		t.Fatal(err)
	}
}

// tenWords is a transcript of three chunks of five words: words 1-5, 4-8 and
// 7-10, as two words of each chunk overlap the next.
const tenWords = "one two three four five six seven eight nine ten"

// TestIndexingASessionReadsOnlyItsEmbeddings fails if indexing a session
// reads every session's stored embeddings, which grow with the whole corpus,
// or reads its own more than once an index, or does not skip the chunks it
// finds stored.
func TestIndexingASessionReadsOnlyItsEmbeddings(t *testing.T) {
	store := &countingStore{SQLiteStore: newTestStore(t)}
	endedSession(t, store.SQLiteStore, "other", "hello from another")
	client := &countingClient{}
	idx := NewIndexer(client, store, 5)
	_, _, generation, err := store.GetCanonicalTranscriptWithGeneration("other")
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.IndexSessionAtGeneration(context.Background(), "other", "hello from another", generation); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession("s", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if generation, err = store.DeletionGeneration("s"); err != nil {
		t.Fatal(err)
	}
	store.all.Store(0)
	store.session.Store(0)
	client.texts.Store(0)

	for range 2 {
		if err := idx.IndexSessionAtGeneration(context.Background(), "s", tenWords, generation); err != nil {
			t.Fatalf("IndexSessionAtGeneration: %v", err)
		}
	}
	if all, session := store.all.Load(), store.session.Load(); all != 0 || session != 2 {
		t.Fatalf("two indexes of one session read every session's embeddings %d times and its own %d; want 0 and 2", all, session)
	}
	if texts := client.texts.Load(); texts != 3 {
		t.Fatalf("two indexes of a session of 3 chunks embedded %d texts; want 3, the second finding each stored", texts)
	}
}

// TestBackfillReadsEveryEmbeddingOnce fails if BackfillMissing reads every
// session's stored embeddings other than once, to find the unfinished
// sessions, whatever the number of sessions it indexes: a read for each
// would read the corpus once more a session. It also fails if a session
// without embeddings or an unfinished one is not finished, or a chunk stored
// before is embedded again.
func TestBackfillReadsEveryEmbeddingOnce(t *testing.T) {
	store := &countingStore{SQLiteStore: newTestStore(t)}
	endedSession(t, store.SQLiteStore, "missing-a", "hello from a")
	endedSession(t, store.SQLiteStore, "missing-b", "hello from b")
	endedSession(t, store.SQLiteStore, "partial", tenWords)
	client := &countingClient{}
	idx := NewIndexer(client, store, 5)
	// partial is left with its first chunk stored and the others not, as an
	// index a refused call stopped is.
	transcript, _, generation, err := store.GetCanonicalTranscriptWithGeneration("partial")
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.IndexSessionAtGeneration(context.Background(), "partial", transcript, generation); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteEmbeddingChunksAtOrAfterGeneration("partial", 1, generation); err != nil {
		t.Fatal(err)
	}
	store.all.Store(0)
	store.session.Store(0)
	client.texts.Store(0)

	if err := idx.BackfillMissing(context.Background()); err != nil {
		t.Fatalf("BackfillMissing: %v", err)
	}
	if all, session := store.all.Load(), store.session.Load(); all != 1 || session != 3 {
		t.Fatalf("a backfill of 3 sessions read every session's embeddings %d times and a session's %d; want 1 and 3", all, session)
	}
	if texts := client.texts.Load(); texts != 4 {
		t.Fatalf("the backfill embedded %d texts; want 4: missing-a's chunk, missing-b's, and partial's last two", texts)
	}
	for id, want := range map[string]int{"missing-a": 1, "missing-b": 1, "partial": 3} {
		if got := storedChunks(t, store.SQLiteStore, id); len(got) != want {
			t.Errorf("%s has chunks %v stored after the backfill; want %d", id, got, want)
		}
	}
}
