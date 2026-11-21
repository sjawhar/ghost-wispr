package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/embedding"
)

// searchIndexWarningKey is the warning of search's index: the embedding
// provider refused an index, so sessions may be missing from search.
const searchIndexWarningKey = "search-index"

// searchQuestionWarningKey is the warning of semantic search's questions:
// the embedding provider refused the last one.
const searchQuestionWarningKey = "search-question"

// searchIndexer is the embedding indexer as the unit runs it. An index the
// provider refuses, at a session's end or in a backfill, puts the index
// warning on /api/status, naming embedding.model and the provider's status
// and code. Only a backfill of the sessions with missing embeddings that
// works clears it: a session's index that works while it is up asks for that
// backfill (requestBackfill), as does a settings save that changes what
// embedding.model signs in with (followSignIn), so the sessions the provider
// refused are indexed with no restart. A question semantic search answers
// leaves it (searchQueries has a warning of its own). An index that fails
// another way (a timeout, a deletion under way) leaves it as it is.
type searchIndexer struct {
	*embedding.Indexer
	ref      string // embedding.model, as the warning names it
	warnings *warningsRegistry
	ctx      context.Context // the unit's: a background backfill ends with it

	mu      sync.Mutex
	refused bool // the index warning is up
	running bool // a background backfill runs
	again   bool // a backfill was asked for while it ran
}

// newSearchIndexer indexes with client, labelling each stored vector model,
// the model as its provider names it; ref is embedding.model. Its background
// backfills run until ctx ends.
func newSearchIndexer(ctx context.Context, client embedding.Client, model, ref string, store embedding.EmbeddingStore, warnings *warningsRegistry) *searchIndexer {
	indexer := embedding.NewIndexer(client, store, 500)
	indexer.SetModel(model)
	return &searchIndexer{Indexer: indexer, ref: ref, warnings: warnings, ctx: ctx}
}

// IndexSessionAtGeneration indexes a session. A refusal puts up the index
// warning; an index that works while it is up asks for the backfill of the
// sessions the provider refused, which clears it once they are indexed.
func (s *searchIndexer) IndexSessionAtGeneration(ctx context.Context, sessionID, transcript string, generation int) error {
	err := s.Indexer.IndexSessionAtGeneration(ctx, sessionID, transcript, generation)
	if err != nil {
		s.refuse(err)
		return err
	}
	s.mu.Lock()
	refused := s.refused
	s.mu.Unlock()
	if refused {
		s.requestBackfill()
	}
	return nil
}

// BackfillMissing indexes every session with no embeddings or with
// unfinished ones. A refusal puts up the index warning; a backfill that
// works takes it down.
func (s *searchIndexer) BackfillMissing(ctx context.Context) error {
	err := s.Indexer.BackfillMissing(ctx)
	if err != nil {
		s.refuse(err)
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refused = false
	s.warnings.Clear(searchIndexWarningKey)
	return nil
}

// refuse puts up the index warning when err is the provider's refusal.
func (s *searchIndexer) refuse(err error) {
	refusal, ok := providerRefusal(err)
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refused = true
	s.warnings.Set(searchIndexWarningKey, "Search cannot index sessions with "+s.ref+": "+refusalWarning(refusal, "check that embedding.model names an embedding model"))
}

// requestBackfill runs BackfillMissing in the background, one run at a time:
// asked while one runs, it runs once more after it, so a key saved while a
// run with the old one is under way is tried too.
func (s *searchIndexer) requestBackfill() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		s.again = true
		return
	}
	s.running = true
	go func() {
		for {
			if err := s.BackfillMissing(s.ctx); err != nil && s.ctx.Err() == nil {
				slog.Warn("embedding backfill failed", "error", err)
			}
			s.mu.Lock()
			if !s.again || s.ctx.Err() != nil {
				s.running, s.again = false, false
				s.mu.Unlock()
				return
			}
			s.again = false
			s.mu.Unlock()
		}
	}()
}

// followSignIn asks for the backfill (requestBackfill) after each settings
// save that changes what embedding.model signs in with (embeddingSignIn): a
// key saved for the provider that refused the sessions indexes them with no
// restart.
func (s *searchIndexer) followSignIn(cfgStore *config.Store) {
	cfg := cfgStore.Get()
	last := embeddingSignIn(&cfg)
	onConfigChange(cfgStore, func(cfg *config.Config) {
		signIn := embeddingSignIn(cfg)
		if signIn == last {
			return
		}
		last = signIn
		// The saved key reaches the indexer's client before the backfill
		// signs in with it.
		unitProviders.of(cfg)
		s.requestBackfill()
	})
}

// embeddingSignIn is what embedding.model signs in with: the model, its
// provider, the provider's key and the Google credential file.
func embeddingSignIn(cfg *config.Config) string {
	m, _ := cfg.ResolveModel(config.RoleEmbedding, cfg.Embedding.Model)
	data, _ := json.Marshal(struct {
		Model    string
		Provider config.Provider
		Key      string
		File     string
	}{cfg.Embedding.Model, m.Config, cfg.APIKey(m.Provider), cfg.GoogleCredentialsFile})
	return string(data)
}

// searchQueries is the embedding client of GET /api/search/semantic: a
// question the provider refuses is logged and puts the question warning on
// /api/status; the next question that works clears it, and only it: the
// index warning stays until the sessions it is about are indexed. Neither the
// log nor the warning holds the provider's message (providererr withholds
// it).
type searchQueries struct {
	client   embedding.Client
	ref      string // embedding.model, as the warning names it
	warnings *warningsRegistry
}

// searchQueryClient is client as semantic search asks it, or nil (no client,
// so the route answers 501) while embeddings are off.
func searchQueryClient(client embedding.Client, ref string, warnings *warningsRegistry) embedding.Client {
	if client == nil {
		return nil
	}
	return &searchQueries{client: client, ref: ref, warnings: warnings}
}

// Embed embeds a question, and warns as the result says.
func (q *searchQueries) Embed(ctx context.Context, texts []string, taskType embedding.TaskType) ([][]float32, error) {
	vectors, err := q.client.Embed(ctx, texts, taskType)
	if err == nil {
		q.warnings.Clear(searchQuestionWarningKey)
		return vectors, nil
	}
	slog.Warn("semantic search: the embedding provider did not answer the question", "model", q.ref, "error", err)
	if refusal, ok := providerRefusal(err); ok {
		q.warnings.Set(searchQuestionWarningKey, "Search cannot answer a question with "+q.ref+": "+refusalWarning(refusal, "check that embedding.model names an embedding model"))
	}
	return nil, err
}

// ReportedModel is the model the provider's last answer named, which the
// search answer reports; "" when the client reports none.
func (q *searchQueries) ReportedModel() string {
	if reporter, ok := q.client.(interface{ ReportedModel() string }); ok {
		return reporter.ReportedModel()
	}
	return ""
}
