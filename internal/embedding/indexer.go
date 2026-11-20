package embedding

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/sjawhar/ghost-wispr/internal/retry"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

const (
	defaultChunkSize = 500
	chunkOverlap     = 50
	defaultModelName = "unknown"
)

type StoredEmbedding = storage.StoredEmbedding

type EmbeddingStore interface {
	StoreEmbeddingAtGeneration(sessionID string, chunkIndex int, vector []float32, textHash, model string, generation int) error
	DeleteEmbeddingChunksAtOrAfterGeneration(sessionID string, chunkIndex, generation int) error
	// GetEmbeddings reads one session's stored chunks, for each index of it;
	// GetAllEmbeddings reads every session's, once a backfill (unfinished).
	GetEmbeddings(sessionID string) ([]StoredEmbedding, error)
	GetAllEmbeddings() ([]StoredEmbedding, error)
	GetSessionsWithoutEmbeddings() ([]string, error)
	GetCanonicalTranscriptWithGeneration(sessionID string) (transcript, source string, generation int, err error)
}

type Indexer struct {
	client      Client
	store       EmbeddingStore
	chunkSize   int
	model       string
	retryPolicy retry.Policy
}

func NewIndexer(client Client, store EmbeddingStore, chunkSize int) *Indexer {
	if chunkSize <= 0 {
		chunkSize = defaultChunkSize
	}
	return &Indexer{client: client, store: store, chunkSize: chunkSize, model: defaultModelName,
		retryPolicy: retry.Policy{MaxRetries: retry.DefaultMaxRetries, RateLimitBudget: retry.DefaultRateLimitBudget}}
}

func (i *Indexer) SetModel(model string) {
	if strings.TrimSpace(model) == "" {
		return
	}
	i.model = model
}

func SplitIntoChunks(text string, chunkSize int) []string {
	words := strings.Fields(text)
	if len(words) == 0 {
		return nil
	}
	if chunkSize <= 0 {
		chunkSize = defaultChunkSize
	}

	overlap := chunkOverlap
	if overlap >= chunkSize {
		overlap = chunkSize / 2
	}

	chunks := make([]string, 0, (len(words)/chunkSize)+1)
	for start := 0; start < len(words); {
		end := start + chunkSize
		if end > len(words) {
			end = len(words)
		}
		chunks = append(chunks, strings.Join(words[start:end], " "))
		if end == len(words) {
			break
		}
		next := end - overlap
		if next <= start {
			next = start + 1
		}
		start = next
	}

	return chunks
}

// IndexSessionAtGeneration indexes only while generation still names the
// transcript the caller read.
func (i *Indexer) IndexSessionAtGeneration(ctx context.Context, sessionID, transcript string, generation int) error {
	return i.indexSession(ctx, sessionID, transcript, generation)
}

// indexSession embeds each chunk of transcript whose stored vector is
// missing, of other text or of another model, one chunk a call, and stores
// each vector as it comes: a call refused past the retry policy fails the
// index with the chunks embedded before it kept, and the next index of the
// session (BackfillMissing finds it) sends only the rest.
func (i *Indexer) indexSession(ctx context.Context, sessionID, transcript string, generation int) error {
	chunks := SplitIntoChunks(transcript, i.chunkSize)
	if len(chunks) == 0 {
		return nil
	}

	// The session's own chunks only: a backfill indexes many sessions, and a
	// read of every session's for each would read the corpus once a session.
	existing, err := i.store.GetEmbeddings(sessionID)
	if err != nil {
		return fmt.Errorf("get embeddings: %w", err)
	}

	byChunk := make(map[int]StoredEmbedding, len(existing))
	for _, emb := range existing {
		byChunk[emb.ChunkIndex] = emb
	}

	// Chunks past the transcript's end go first, so none outlives a failure.
	if err := i.store.DeleteEmbeddingChunksAtOrAfterGeneration(sessionID, len(chunks), generation); err != nil {
		return err
	}
	for idx, chunk := range chunks {
		h := textHash(chunk)
		if emb, ok := byChunk[idx]; ok && emb.TextHash == h && emb.Model == i.model {
			continue
		}
		var vectors [][]float32
		if err := retry.DoWith(ctx, func() error {
			var embedErr error
			vectors, embedErr = i.client.Embed(ctx, []string{chunk}, TaskTypeDocument)
			return embedErr
		}, i.retryPolicy); err != nil {
			return fmt.Errorf("embed chunk %d: %w", idx, err)
		}
		if len(vectors) != 1 {
			return fmt.Errorf("embed returned %d vectors for chunk %d", len(vectors), idx)
		}
		if err := i.store.StoreEmbeddingAtGeneration(sessionID, idx, vectors[0], h, i.model, generation); err != nil {
			return fmt.Errorf("store embedding chunk %d: %w", idx, err)
		}
	}
	return nil
}

// BackfillMissing indexes each ended session with no embeddings, and each
// session whose stored chunks do not cover its transcript (unfinished), two
// at a time. It returns only once every worker it started has ended, also
// when ctx ends first: a worker writes the store, which its caller may close
// next.
func (i *Indexer) BackfillMissing(ctx context.Context) error {
	ids, err := i.store.GetSessionsWithoutEmbeddings()
	if err != nil {
		return fmt.Errorf("get sessions without embeddings: %w", err)
	}
	unfinished, err := i.unfinished()
	if err != nil {
		return err
	}
	ids = append(ids, unfinished...)
	if len(ids) == 0 {
		return nil
	}

	sem := make(chan struct{}, 2)
	var wg sync.WaitGroup
	var firstErr atomic.Value

	for _, id := range ids {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return ctx.Err()
		}

		wg.Add(1)
		go func(sessionID string) {
			defer wg.Done()
			defer func() { <-sem }()

			transcript, _, generation, err := i.store.GetCanonicalTranscriptWithGeneration(sessionID)
			if err != nil {
				slog.Error("embedding backfill failed to load canonical transcript", "session_id", sessionID, "error", err)
				setFirstErr(&firstErr, fmt.Errorf("get canonical transcript for %s: %w", sessionID, err))
				return
			}

			if err := i.IndexSessionAtGeneration(ctx, sessionID, transcript, generation); err != nil {
				slog.Error("embedding backfill failed", "session_id", sessionID, "error", err)
				setFirstErr(&firstErr, fmt.Errorf("index session %s: %w", sessionID, err))
			}
		}(id)
	}

	wg.Wait()
	if v := firstErr.Load(); v != nil {
		return v.(error)
	}

	return nil
}

// unfinished are the sessions with stored chunks that do not cover their
// transcript: a chunk missing or of other text, or one past its end. An
// index a refused call stopped part way leaves one, as does a re-index that
// failed. The model is not compared: a session whose chunks a former model
// embedded is embedded again when it is next indexed, as before.
func (i *Indexer) unfinished() ([]string, error) {
	all, err := i.store.GetAllEmbeddings()
	if err != nil {
		return nil, fmt.Errorf("get embeddings: %w", err)
	}
	hashes := map[string]map[int]string{} // session → chunk index → text hash
	for _, emb := range all {
		if hashes[emb.SessionID] == nil {
			hashes[emb.SessionID] = map[int]string{}
		}
		hashes[emb.SessionID][emb.ChunkIndex] = emb.TextHash
	}
	var ids []string
	for _, sessionID := range slices.Sorted(maps.Keys(hashes)) {
		transcript, _, _, err := i.store.GetCanonicalTranscriptWithGeneration(sessionID)
		if err != nil {
			slog.Warn("embedding backfill cannot read a transcript to check its chunks", "session_id", sessionID, "error", err)
			continue
		}
		if !covers(hashes[sessionID], SplitIntoChunks(transcript, i.chunkSize)) {
			ids = append(ids, sessionID)
		}
	}
	return ids, nil
}

// covers says whether stored, chunk index → text hash, holds chunks and no
// more. An empty transcript has nothing to index.
func covers(stored map[int]string, chunks []string) bool {
	if len(chunks) == 0 {
		return true
	}
	if len(stored) != len(chunks) {
		return false
	}
	for idx, chunk := range chunks {
		if stored[idx] != textHash(chunk) {
			return false
		}
	}
	return true
}

func setFirstErr(slot *atomic.Value, err error) {
	if err == nil {
		return
	}
	if slot.Load() != nil {
		return
	}
	slot.Store(err)
}

func textHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}
