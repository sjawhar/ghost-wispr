package embedding

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
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
	GetAllEmbeddings() ([]StoredEmbedding, error)
	GetSessionsWithoutEmbeddings() ([]string, error)
	GetCanonicalTranscriptWithGeneration(sessionID string) (transcript, source string, generation int, err error)
}

type Indexer struct {
	client    Client
	store     EmbeddingStore
	chunkSize int
	model     string
}

func NewIndexer(client Client, store EmbeddingStore, chunkSize int) *Indexer {
	if chunkSize <= 0 {
		chunkSize = defaultChunkSize
	}
	return &Indexer{client: client, store: store, chunkSize: chunkSize, model: defaultModelName}
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

func (i *Indexer) indexSession(ctx context.Context, sessionID, transcript string, generation int) error {
	chunks := SplitIntoChunks(transcript, i.chunkSize)
	if len(chunks) == 0 {
		return nil
	}

	existing, err := i.store.GetAllEmbeddings()
	if err != nil {
		return fmt.Errorf("get embeddings: %w", err)
	}

	byChunk := make(map[int]StoredEmbedding, len(chunks))
	for _, emb := range existing {
		if emb.SessionID != sessionID {
			continue
		}
		byChunk[emb.ChunkIndex] = emb
	}

	texts := make([]string, 0, len(chunks))
	indexes := make([]int, 0, len(chunks))
	hashes := make([]string, 0, len(chunks))
	for idx, chunk := range chunks {
		h := textHash(chunk)
		if emb, ok := byChunk[idx]; ok && emb.TextHash == h && emb.Model == i.model {
			continue
		}
		texts = append(texts, chunk)
		indexes = append(indexes, idx)
		hashes = append(hashes, h)
	}

	if len(texts) == 0 {
		return i.store.DeleteEmbeddingChunksAtOrAfterGeneration(sessionID, len(chunks), generation)
	}

	var vectors [][]float32
	if err := retry.Do(ctx, func() error {
		var embedErr error
		vectors, embedErr = i.client.Embed(ctx, texts, TaskTypeDocument)
		return embedErr
	}, retry.DefaultMaxRetries); err != nil {
		return fmt.Errorf("embed chunks: %w", err)
	}
	if len(vectors) != len(texts) {
		return fmt.Errorf("embed returned %d vectors for %d chunks", len(vectors), len(texts))
	}

	for idx := range indexes {
		if err := i.store.StoreEmbeddingAtGeneration(sessionID, indexes[idx], vectors[idx], hashes[idx], i.model, generation); err != nil {
			return fmt.Errorf("store embedding chunk %d: %w", indexes[idx], err)
		}
	}
	if err := i.store.DeleteEmbeddingChunksAtOrAfterGeneration(sessionID, len(chunks), generation); err != nil {
		return err
	}
	return nil
}

func (i *Indexer) BackfillMissing(ctx context.Context) error {
	ids, err := i.store.GetSessionsWithoutEmbeddings()
	if err != nil {
		return fmt.Errorf("get sessions without embeddings: %w", err)
	}
	if len(ids) == 0 {
		return nil
	}


	sem := make(chan struct{}, 2)
	var wg sync.WaitGroup
	var firstErr atomic.Value

	for _, id := range ids {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		wg.Add(1)
		go func(sessionID string) {
			defer wg.Done()

			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
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
