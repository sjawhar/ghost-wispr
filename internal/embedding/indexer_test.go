package embedding

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

type mockEmbedClient struct {
	calls     [][]string
	taskTypes []TaskType
}

func (m *mockEmbedClient) Embed(_ context.Context, texts []string, taskType TaskType) ([][]float32, error) {
	batch := append([]string(nil), texts...)
	m.calls = append(m.calls, batch)
	m.taskTypes = append(m.taskTypes, taskType)

	vectors := make([][]float32, len(texts))
	for i := range texts {
		vectors[i] = []float32{float32(i + 1), float32(len(texts))}
	}
	return vectors, nil
}

func newTestStore(t *testing.T) *storage.SQLiteStore {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	store, err := storage.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("create sqlite store: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})
	return store
}

func makeTranscript(words int) string {
	parts := make([]string, 0, words)
	for i := 0; i < words; i++ {
		parts = append(parts, fmt.Sprintf("w%d", i))
	}
	return strings.Join(parts, " ")
}

func TestSplitIntoChunks(t *testing.T) {
	transcript := makeTranscript(1200)
	chunks := SplitIntoChunks(transcript, 500)

	if len(chunks) != 3 {
		t.Fatalf("expected 3 chunks, got %d", len(chunks))
	}

	first := strings.Fields(chunks[0])
	second := strings.Fields(chunks[1])
	third := strings.Fields(chunks[2])

	if len(first) != 500 {
		t.Fatalf("expected first chunk length 500, got %d", len(first))
	}
	if len(second) != 500 {
		t.Fatalf("expected second chunk length 500, got %d", len(second))
	}
	if len(third) != 300 {
		t.Fatalf("expected third chunk length 300, got %d", len(third))
	}

	if second[0] != "w450" {
		t.Fatalf("expected second chunk to start at overlap word w450, got %q", second[0])
	}
	if third[0] != "w900" {
		t.Fatalf("expected third chunk to start at overlap word w900, got %q", third[0])
	}
}

func TestIndexerOnSessionEnd(t *testing.T) {
	store := newTestStore(t)
	client := &mockEmbedClient{}
	indexer := NewIndexer(client, store, 500)

	sessionID := "indexer-session-1"
	if err := store.CreateSession(sessionID, time.Now().UTC()); err != nil {
		t.Fatalf("create session: %v", err)
	}

	transcript := makeTranscript(700)
	if err := indexer.IndexSession(context.Background(), sessionID, transcript); err != nil {
		t.Fatalf("IndexSession: %v", err)
	}

	if err := indexer.IndexSession(context.Background(), sessionID, transcript); err != nil {
		t.Fatalf("IndexSession idempotent re-run: %v", err)
	}

	embeddings, err := store.GetEmbeddings(sessionID)
	if err != nil {
		t.Fatalf("get embeddings: %v", err)
	}
	if len(embeddings) != 2 {
		t.Fatalf("expected 2 embeddings (700 words, 500 chunk size, 50 overlap), got %d", len(embeddings))
	}

	if len(client.calls) != 1 {
		t.Fatalf("expected one embed API call due to idempotency, got %d", len(client.calls))
	}
	if len(client.taskTypes) != 1 || client.taskTypes[0] != TaskTypeDocument {
		t.Fatalf("expected document task type, got %#v", client.taskTypes)
	}
}

func TestBackfillMissing(t *testing.T) {
	store := newTestStore(t)
	client := &mockEmbedClient{}
	indexer := NewIndexer(client, store, 500)

	indexedID := "session-indexed"
	missingID := "session-missing"

	for _, id := range []string{indexedID, missingID} {
		if err := store.CreateSession(id, time.Now().UTC()); err != nil {
			t.Fatalf("create session %s: %v", id, err)
		}
		if err := store.EndSession(id, time.Now().UTC(), ""); err != nil {
			t.Fatalf("end session %s: %v", id, err)
		}
		if err := store.AppendSegment(id, transcribe.Segment{Text: "hello world from " + id, Timestamp: time.Now().UTC()}); err != nil {
			t.Fatalf("append segment %s: %v", id, err)
		}
		if err := store.Canonicalize(id); err != nil {
			t.Fatalf("canonicalize %s: %v", id, err)
		}
	}

	if err := indexer.IndexSession(context.Background(), indexedID, "hello world from "+indexedID); err != nil {
		t.Fatalf("pre-index indexed session: %v", err)
	}

	client.calls = nil

	if err := indexer.BackfillMissing(context.Background()); err != nil {
		t.Fatalf("BackfillMissing: %v", err)
	}

	if len(client.calls) != 1 {
		t.Fatalf("expected exactly one embed call for missing session, got %d", len(client.calls))
	}
	if len(client.taskTypes) == 0 {
		t.Fatal("expected at least one embed call for missing session")
	}
	for _, tt := range client.taskTypes {
		if tt != TaskTypeDocument {
			t.Fatalf("expected document task type, got %#v", client.taskTypes)
		}
	}

	indexedEmbeddings, err := store.GetEmbeddings(indexedID)
	if err != nil {
		t.Fatalf("get indexed embeddings: %v", err)
	}
	missingEmbeddings, err := store.GetEmbeddings(missingID)
	if err != nil {
		t.Fatalf("get missing embeddings: %v", err)
	}

	if len(indexedEmbeddings) != 1 {
		t.Fatalf("expected already-indexed session to remain with 1 embedding, got %d", len(indexedEmbeddings))
	}
	if len(missingEmbeddings) != 1 {
		t.Fatalf("expected missing session to be indexed with 1 embedding, got %d", len(missingEmbeddings))
	}
}

func TestIndexerReembedsWhenModelChanges(t *testing.T) {
	store := newTestStore(t)
	client := &mockEmbedClient{}
	indexer := NewIndexer(client, store, 500)

	sessionID := "indexer-session-model-change"
	if err := store.CreateSession(sessionID, time.Now().UTC()); err != nil {
		t.Fatalf("create session: %v", err)
	}

	transcript := makeTranscript(100)
	indexer.SetModel("gemini/gemini-embedding-001")
	if err := indexer.IndexSession(context.Background(), sessionID, transcript); err != nil {
		t.Fatalf("initial IndexSession: %v", err)
	}

	indexer.SetModel("gemini/gemini-embedding-2")
	if err := indexer.IndexSession(context.Background(), sessionID, transcript); err != nil {
		t.Fatalf("reindex after model change: %v", err)
	}

	if len(client.calls) != 2 {
		t.Fatalf("expected re-embed after model change, got %d embed calls", len(client.calls))
	}
	for _, taskType := range client.taskTypes {
		if taskType != TaskTypeDocument {
			t.Fatalf("expected document task type for reindex, got %#v", client.taskTypes)
		}
	}

	embeddings, err := store.GetEmbeddings(sessionID)
	if err != nil {
		t.Fatalf("get embeddings: %v", err)
	}
	if len(embeddings) != 1 {
		t.Fatalf("expected 1 embedding row, got %d", len(embeddings))
	}
	if embeddings[0].Model != "gemini/gemini-embedding-2" {
		t.Fatalf("expected updated model, got %q", embeddings[0].Model)
	}
}
