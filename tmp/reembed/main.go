package main

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/embedding"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

func main() {
	configPath := "/opt/ghost-wispr/ghost-wispr.yaml"
	envPath := filepath.Join(filepath.Dir(configPath), ".env")

	cfgStore, _, err := config.NewStoreWithEnv(configPath, envPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	cfg := cfgStore.Get()
	configDir := filepath.Dir(configPath)

	if cfg.DBPath == "" {
		log.Fatal("db_path is empty")
	}
	if cfg.EmbeddingModel == "" {
		log.Fatal("embedding_model is empty")
	}
	if !filepath.IsAbs(cfg.DBPath) {
		cfg.DBPath = filepath.Join(configDir, cfg.DBPath)
	}

	store, err := storage.NewSQLiteStore(cfg.DBPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer func() { _ = store.Close() }()

	client, err := embedding.NewClient(cfg.EmbeddingModel, embedding.WithGenAIConfig(embedding.GenAIConfig{
		Backend:  cfg.GenAIBackend,
		Project:  cfg.GCPProject,
		Location: cfg.GCPLocation,
	}))
	if err != nil {
		log.Fatalf("new embedding client: %v", err)
	}

	indexer := embedding.NewIndexer(client, store, 500)
	indexer.SetModel(cfg.EmbeddingModel)

	before, err := store.GetAllEmbeddings()
	if err != nil {
		log.Fatalf("list embeddings before delete: %v", err)
	}

	sessionIDs := make(map[string]struct{}, len(before))
	for _, emb := range before {
		sessionIDs[emb.SessionID] = struct{}{}
	}

	for sessionID := range sessionIDs {
		if err := store.DeleteEmbeddings(sessionID); err != nil {
			log.Fatalf("delete embeddings for %s: %v", sessionID, err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()

	if err := indexer.BackfillMissing(ctx); err != nil {
		log.Fatalf("backfill missing: %v", err)
	}

	after, err := store.GetAllEmbeddings()
	if err != nil {
		log.Fatalf("list embeddings after backfill: %v", err)
	}

	fmt.Printf("deleted_sessions=%d embeddings_before=%d embeddings_after=%d model=%s\n", len(sessionIDs), len(before), len(after), cfg.EmbeddingModel)
}
