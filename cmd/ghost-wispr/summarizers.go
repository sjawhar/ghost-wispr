package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync/atomic"
	"time"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/session"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/summary"
)

// errSummariesOff is why a summary path does nothing: no summarization model
// names a provider that can sign in (canSummarize).
var errSummariesOff = errors.New("summarization not configured")

// summarizers holds the summarizer of the summarization settings saved last,
// or none while summaries cannot run (canSummarize). Every summary path reads
// it at each call: the session manager's (follow gives it there), a
// resummarize, the presets list, a summary batch and a merged session's
// summary. A settings change of summarization.model or its presets reaches
// each of them, and summaries turned off stop each of them.
type summarizers struct {
	clients  summary.ClientFactory
	onResult func(error) // every summarizer built reports each result here
	current  atomic.Pointer[summary.Summarizer]
}

// newSummarizers holds no summarizer until the first apply.
func newSummarizers(clients summary.ClientFactory, onResult func(error)) *summarizers {
	return &summarizers{clients: clients, onResult: onResult}
}

// apply makes the summarizer of cfg's summarization settings the current
// one, or none when summaries cannot run, and returns it as the session
// manager takes it: a nil interface while summaries are off, so the manager
// sees no summarizer rather than a nil one.
func (s *summarizers) apply(cfg *config.Config) session.Summarizer {
	if !canSummarize(cfg) {
		s.current.Store(nil)
		return nil
	}
	built := summary.New(cfg.Summarization, s.clients)
	built.OnResult = s.onResult
	s.current.Store(built)
	return built
}

// follow applies each settings change, and gives the session manager the
// summarizer it makes.
func (s *summarizers) follow(cfgStore *config.Store, manager interface{ SetSummarizer(session.Summarizer) }) {
	onConfigChange(cfgStore, func(cfg *config.Config) {
		manager.SetSummarizer(s.apply(cfg))
		if s.get() == nil {
			log.Printf("config: summaries off: no summarization model names a provider that can sign in")
			return
		}
		log.Printf("config: summarizer updated for %s", cfg.Summarization.Model)
	})
}

// get is the current summarizer, nil while summaries are off.
func (s *summarizers) get() *summary.Summarizer { return s.current.Load() }

// presets are the current summarizer's presets, none while summaries are off.
func (s *summarizers) presets() map[string]config.Preset {
	if current := s.get(); current != nil {
		return current.Presets()
	}
	return nil
}

// resummarize makes session sessionID's summary again with the current
// summarizer (resummarize, the function).
func (s *summarizers) resummarize(ctx context.Context, store *storage.SQLiteStore, hub summaryBroadcaster, publish func(sessionID string), sessionID, preset string) error {
	current := s.get()
	if current == nil {
		return errSummariesOff
	}
	return resummarize(ctx, store, current, hub, publish, sessionID, preset)
}

// summarizeMerged makes a merged session's summary with the current
// summarizer, or, while summaries are off, with fallback: the session
// manager's, which then completes it empty.
func (s *summarizers) summarizeMerged(ctx context.Context, store *storage.SQLiteStore, hub summaryBroadcaster, publish func(sessionID string), sessionID string, startedAt time.Time, fallback func(context.Context, string, time.Time) error) error {
	current := s.get()
	if current == nil {
		return fallback(ctx, sessionID, startedAt)
	}
	summarizeMergedSession(ctx, store, current, hub, publish, sessionID)
	return nil
}

// batchRequests are a summary batch's requests for sessions, with the
// current summarizer's presets.
func (s *summarizers) batchRequests(ctx context.Context, sessions []summary.BatchSession) ([]*genai.InlinedRequest, error) {
	current := s.get()
	if current == nil {
		return nil, errSummariesOff
	}
	return current.BuildBatchRequests(ctx, sessions)
}

// batchResults reads a finished summary batch's results. While summaries are
// off, each session's result is a failure that says so: no summary is
// stored, the session keeps the one it has (failSummary), and a start with
// summaries on makes it again.
func (s *summarizers) batchResults(job *genai.BatchJob, sessions []summary.BatchSession) []summary.BatchResult {
	if current := s.get(); current != nil {
		return current.ProcessBatchResults(job, sessions)
	}
	results := make([]summary.BatchResult, 0, len(sessions))
	for _, sess := range sessions {
		if sess.ID != "" {
			results = append(results, summary.BatchResult{SessionID: sess.ID, Error: fmt.Errorf("the batch finished after summaries were turned off: %w", errSummariesOff)})
		}
	}
	return results
}
