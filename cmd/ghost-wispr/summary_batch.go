package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"strings"
	"time"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/providererr"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/summary"
)

const batchResultDiscarded = "discarded"

// newSummaryBatchSubmitter is the summary batch submit of the unit's backlog
// and of POST /api/sessions/batch-backfill: it sends sessionIDs' summaries as
// one batch job of the summarization provider of the settings saved last,
// and returns the job's name and the sessions it holds. A session whose
// summary is being made now, or was made since it was listed, is left out
// (storage's StartBackfillSummary); with none left it sends no job. A
// provider that takes no batch job returns errNoBatchProvider before any
// session starts. A batch job that is not taken after the sessions started
// (the provider refused it) fails each at the generation read with its
// text, keeping its last good summary (failSummary): none is left running,
// which nothing would end.
func newSummaryBatchSubmitter(store *storage.SQLiteStore, hub summaryBroadcaster, summaries *summarizers, cfgStore interface{ Get() config.Config }) func(ctx context.Context, sessionIDs []string) (string, int, error) {
	return func(ctx context.Context, sessionIDs []string) (string, int, error) {
		if summaries.get() == nil {
			return "", 0, errSummariesOff
		}
		latestCfg := cfgStore.Get()
		clientConfig, model, err := batchClientConfig(&latestCfg)
		if err != nil {
			return "", 0, err
		}

		sessions, generations, emptyTranscripts, err := readBatchSummarySessions(store, sessionIDs)
		if err != nil {
			return "", 0, err
		}
		completeEmptySummaries(store, emptyTranscripts)
		if len(sessions) == 0 {
			return "", 0, fmt.Errorf("no sessions with transcripts found for backfill")
		}
		started := make([]string, 0, len(sessions))
		claims := make(map[string]storage.SummaryClaim, len(sessions))
		fail := func(err error) (string, int, error) {
			for _, id := range started {
				failBatchClaim(store, hub, id, claims[id], err)
			}
			return "", 0, err
		}
		taken := sessions[:0]
		for _, session := range sessions {
			attempt, err := store.StartBatchSummary(session.ID)
			if errors.Is(err, sql.ErrNoRows) {
				// Being made now, or made since the backlog listed it: it
				// is not this job's (summarizeBackfillSession).
				continue
			}
			if err != nil {
				return fail(fmt.Errorf("start batch summary for %s: %w", session.ID, err))
			}
			claims[session.ID] = storage.SummaryClaim{Generation: generations[session.ID], Attempt: attempt}
			started = append(started, session.ID)
			taken = append(taken, session)
		}
		sessions = taken
		if len(sessions) == 0 {
			return "", 0, nil
		}

		requests, err := summaries.batchRequests(ctx, sessions)
		if err != nil {
			return fail(fmt.Errorf("build batch requests: %w", err))
		}

		genaiClient, err := genai.NewClient(ctx, clientConfig)
		if err != nil {
			return fail(fmt.Errorf("create genai client: %w", err))
		}

		job, err := genaiClient.Batches.Create(ctx, model, &genai.BatchJobSource{
			InlinedRequests: requests,
		}, nil)
		if err != nil {
			// The refusal names the provider as the config names it.
			provider, _, _ := config.ParseModelRef(latestCfg.Summarization.Model)
			return fail(fmt.Errorf("create batch job: %w", providererr.From(provider, err)))
		}
		provider, _, _ := config.ParseModelRef(latestCfg.Summarization.Model)
		if err := store.RecordSummaryBatch(job.Name, provider, claims); err != nil {
			// Unrecorded, the job's results cannot be stored.
			return fail(fmt.Errorf("persist summary batch %s: %w", job.Name, err))
		}

		return job.Name, len(sessions), nil
	}
}

// newSummaryBatchPoller is the poll of a summary batch job, by the start's
// backlog and by GET /api/batches/{name}: it reads the job from the
// provider that took it (batchPollClientConfig), signed in as the settings
// saved last have it, and, once the job has ended, stores its results at
// their claims (applySummaryBatchResults). A job this unit never submitted
// is storage's ErrUnknownSummaryBatch, which the route answers 404. A job
// whose provider is gone from the settings, speaks another API now or has
// no key cannot be asked about: its sessions fail with the reason, and a
// backlog takes them.
func newSummaryBatchPoller(store *storage.SQLiteStore, hub summaryBroadcaster, summaries *summarizers, cfgStore interface{ Get() config.Config }, publish func(sessionID string)) func(ctx context.Context, batchName string) (map[string]any, error) {
	return func(ctx context.Context, batchName string) (map[string]any, error) {
		latestCfg := cfgStore.Get()
		took, err := store.SummaryBatchProvider(batchName)
		if err != nil {
			return nil, err
		}
		// Its refusals name the provider as the config names it.
		clientConfig, provider, err := batchPollClientConfig(&latestCfg, took)
		if errors.Is(err, errBatchProviderGone) {
			// The job cannot be asked about: its state is unknown, and the
			// unit gives it up (abandoned), failing its claims.
			reason := strings.TrimPrefix(err.Error(), errBatchProviderGone.Error()+": ")
			failed, err := failPendingBatchSessions(store, hub, batchName, errors.New(reason))
			if err != nil {
				return nil, err
			}
			return map[string]any{"name": batchName, "abandoned": true, "failed": failed, "total": failed, "error": reason}, nil
		}
		if err != nil {
			return nil, err
		}

		genaiClient, err := genai.NewClient(ctx, clientConfig)
		if err != nil {
			return nil, fmt.Errorf("create genai client: %w", err)
		}

		job, err := genaiClient.Batches.Get(ctx, batchName, nil)
		if err != nil {
			return nil, fmt.Errorf("get batch job: %w", providererr.From(provider, err))
		}

		result := map[string]any{
			"name":  job.Name,
			"state": string(job.State),
		}

		if job.State == genai.JobStateSucceeded || job.State == genai.JobStatePartiallySucceeded {
			var sessions []summary.BatchSession
			if job.Dest != nil {
				for _, resp := range job.Dest.InlinedResponses {
					id := ""
					if resp.Metadata != nil {
						id = resp.Metadata["session_id"]
					}
					transcript := ""
					if id != "" {
						if loaded, ok := loadSessionTranscript(store, id); ok {
							transcript = loaded
						}
					}
					sessions = append(sessions, summary.BatchSession{ID: id, Transcript: transcript})
				}
			}
			batchResults := summaries.batchResults(job, sessions)
			if len(batchResults) == 0 {
				slog.Warn("summary batch completed without inline results; marking submitted sessions failed", "batch", batchName)
			}
			succeeded, failed, err := applySummaryBatchResults(store, hub, publish, batchName, batchResults)
			if err != nil {
				return nil, err
			}
			result["succeeded"] = succeeded
			result["failed"] = failed
			result["total"] = succeeded + failed
		}

		if reason, ended := unsuccessfulBatchEnd(job.State); ended {
			// Gemini's message can quote the requests, the transcripts: the
			// reason keeps the error's code's name.
			if job.Error != nil && job.Error.Code != nil {
				reason += ": " + providererr.RPC(provider, *job.Error.Code, job.Error.Message).Error()
			}
			failed, err := failPendingBatchSessions(store, hub, batchName, errors.New(reason))
			if err != nil {
				return nil, err
			}
			result["failed"] = failed
			result["total"] = failed
			result["error"] = reason
		}

		return result, nil
	}
}

// unsuccessfulBatchEnd says whether a batch job in state has ended without
// results, and how: failed, cancelled or expired, the states genai names
// final short of success. A paused job can resume, and a cancelling one can
// still succeed or fail.
func unsuccessfulBatchEnd(state genai.JobState) (reason string, ended bool) {
	switch state {
	case genai.JobStateFailed:
		return "the batch job failed", true
	case genai.JobStateCancelled:
		return "the batch job was cancelled", true
	case genai.JobStateExpired:
		return "the batch job expired", true
	}
	return "", false
}

// failPendingBatchSessions fails, with cause, the summary of each session of
// batch job batchName whose result is still pending, at the job's claim
// (failBatchSummary): a job that ended without results leaves none of its
// sessions running, which no backlog takes, and changes no summary started
// since it claimed the session.
func failPendingBatchSessions(store *storage.SQLiteStore, hub summaryBroadcaster, batchName string, cause error) (int, error) {
	sessions, err := store.SummaryBatchSessions(batchName)
	if err != nil {
		return 0, err
	}
	failed := 0
	for sessionID, batchSession := range sessions {
		if batchSession.State != "pending" {
			continue
		}
		if err := failBatchSummary(store, hub, batchName, sessionID, batchSession.SummaryClaim, cause); err != nil {
			return failed, err
		}
		failed++
	}
	return failed, nil
}

// pollStartupSummaryBatch polls the start's summary batch job batchName
// (poll) every interval until it has ended, with results or without them,
// the unit gave it up (its provider can no longer be asked about it), or ctx
// ends.
func pollStartupSummaryBatch(ctx context.Context, poll func(context.Context, string) (map[string]any, error), batchName string, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		result, err := poll(ctx, batchName)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Warn("startup batch job poll failed", "batch", batchName, "error", err)
		} else if abandoned, _ := result["abandoned"].(bool); abandoned {
			reason, _ := result["error"].(string)
			failed, _ := result["failed"].(int)
			log.Printf("startup: batch job %s given up (%s): %d sessions failed", batchName, reason, failed)
			return
		} else if state, _ := result["state"].(string); state != "" {
			succeeded, _ := result["succeeded"].(int)
			failed, _ := result["failed"].(int)
			if state == string(genai.JobStateSucceeded) || state == string(genai.JobStatePartiallySucceeded) {
				log.Printf("startup: batch job %s complete: %d succeeded, %d failed", batchName, succeeded, failed)
				return
			}
			if _, ended := unsuccessfulBatchEnd(genai.JobState(state)); ended {
				reason, _ := result["error"].(string)
				log.Printf("startup: batch job %s ended without results (%s): %d sessions failed", batchName, reason, failed)
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// readBatchSummarySessions reads every submitted transcript and deletion
// generation from the same SQLite row, so a delete cannot pair old text with a
// newer generation. Each is submitted as a summary reads it, each segment
// after its speaker's label (CanonicalizeForSummary). Sessions whose
// transcript is empty are not submitted: emptyTranscripts maps each to the
// generation read with its transcript.
func readBatchSummarySessions(store *storage.SQLiteStore, sessionIDs []string) (sessions []summary.BatchSession, generations, emptyTranscripts map[string]int, err error) {
	sessions = make([]summary.BatchSession, 0, len(sessionIDs))
	generations = make(map[string]int, len(sessionIDs))
	emptyTranscripts = make(map[string]int)
	for _, sessionID := range sessionIDs {
		transcript, labeled, generation, err := store.CanonicalizeForSummary(sessionID)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("canonicalize batch summary input for %s: %w", sessionID, err)
		}
		if strings.TrimSpace(transcript) == "" {
			emptyTranscripts[sessionID] = generation
			continue
		}
		sessions = append(sessions, summary.BatchSession{ID: sessionID, Transcript: labeled})
		generations[sessionID] = generation
	}
	return sessions, generations, emptyTranscripts, nil
}

// completeEmptySummaries completes, empty, the summary of each session whose
// transcript was empty (emptyTranscripts, with the generation read with it):
// an empty summary keeps the session's summary. A session whose text changed
// since is left to the summary run of that change.
func completeEmptySummaries(store *storage.SQLiteStore, emptyTranscripts map[string]int) {
	for id, generation := range emptyTranscripts {
		if err := store.StartEmptySummaryAtGeneration(id, generation); err != nil {
			continue
		}
		_ = store.UpdateSummaryAtGeneration(id, "", "", storage.SummaryCompleted, "", "{}", "", generation)
	}
}

// applySummaryBatchResults stores each terminal response only at its claim:
// the generation the job read, while the job's summary attempt is still the
// session's (storage's UpdateSummaryAtClaim). Missing responses are
// failures; already-handled sessions are skipped so a repeated poll cannot
// publish a result twice. A result or failure whose claim is stale is
// dropped with a log line (dropStaleBatchResult).
func applySummaryBatchResults(store *storage.SQLiteStore, hub summaryBroadcaster, publish func(string), batchName string, results []summary.BatchResult) (succeeded, failed int, err error) {
	sessions, err := store.SummaryBatchSessions(batchName)
	if err != nil {
		return 0, 0, err
	}
	handled := make(map[string]struct{}, len(results))
	for _, result := range results {
		batchSession, ok := sessions[result.SessionID]
		if !ok || batchSession.State != "pending" {
			continue
		}
		handled[result.SessionID] = struct{}{}
		if result.Error != nil {
			if err := failBatchSummary(store, hub, batchName, result.SessionID, batchSession.SummaryClaim, result.Error); err != nil {
				return succeeded, failed, err
			}
			failed++
			continue
		}
		if err := store.UpdateSummaryAtClaim(result.SessionID, result.Title, result.Summary, storage.SummaryCompleted, result.PresetName, result.SpeakerNames, result.Model, batchSession.SummaryClaim); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				if err := dropStaleBatchResult(store, batchName, result.SessionID); err != nil {
					return succeeded, failed, err
				}
				failed++
				continue
			}
			return succeeded, failed, fmt.Errorf("store batch summary for %s: %w", result.SessionID, err)
		}
		if err := store.MarkSummaryBatchSessionDone(batchName, result.SessionID, storage.SummaryCompleted); err != nil {
			return succeeded, failed, err
		}
		if hub != nil {
			hub.BroadcastSummaryReady(result.SessionID, result.Title, result.Summary, storage.SummaryCompleted, result.PresetName)
		}
		if publish != nil {
			publish(result.SessionID)
		}
		succeeded++
	}
	for sessionID, batchSession := range sessions {
		if batchSession.State != "pending" {
			continue
		}
		if _, ok := handled[sessionID]; ok {
			continue
		}
		if err := failBatchSummary(store, hub, batchName, sessionID, batchSession.SummaryClaim, errors.New("batch job returned no result for session")); err != nil {
			return succeeded, failed, err
		}
		failed++
	}
	return succeeded, failed, nil
}

// failBatchSummary records that batch job batchName's claim of session
// sessionID's summary failed (cause), only while the claim is still the
// session's summary attempt (storage's FailSummaryAtClaim), and marks the
// claim done. A stale claim is dropped with a log line
// (dropStaleBatchResult): the summary started since stays as it is.
func failBatchSummary(store *storage.SQLiteStore, hub summaryBroadcaster, batchName, sessionID string, claim storage.SummaryClaim, cause error) error {
	kept, err := store.FailSummaryAtClaim(sessionID, fmt.Sprintf("summarization failed: %v", cause), claim)
	if errors.Is(err, sql.ErrNoRows) {
		return dropStaleBatchResult(store, batchName, sessionID)
	}
	if err != nil {
		return err
	}
	if err := store.MarkSummaryBatchSessionDone(batchName, sessionID, storage.SummaryFailed); err != nil {
		return err
	}
	if hub != nil {
		hub.BroadcastSummaryFailed(sessionID, kept.Title, kept.Summary, kept.SummaryPreset, kept.SummaryError)
	}
	return nil
}

// dropStaleBatchResult marks batch job batchName's claim of session
// sessionID discarded, and logs it: the session's summary was started again
// (the start's backlog after a restart, a Resummarize), or its transcript
// changed, since the job claimed it, so the job's result or failure is not
// the session's.
func dropStaleBatchResult(store *storage.SQLiteStore, batchName, sessionID string) error {
	slog.Warn("dropped a stale summary batch result: the session's summary was started again, or its transcript changed, since the job claimed it", "batch", batchName, "session_id", sessionID)
	return store.MarkSummaryBatchSessionDone(batchName, sessionID, batchResultDiscarded)
}

// failBatchClaim records that the summary batch job being submitted could
// not take session sessionID's summary (cause), only while its claim is still
// the session's summary attempt, and announces on hub (when not nil) the
// title and summary the session keeps with the reason. The job holds no
// record yet: a stale claim is only logged.
func failBatchClaim(store *storage.SQLiteStore, hub summaryBroadcaster, sessionID string, claim storage.SummaryClaim, cause error) {
	kept, err := store.FailSummaryAtClaim(sessionID, fmt.Sprintf("summarization failed: %v", cause), claim)
	if errors.Is(err, sql.ErrNoRows) {
		slog.Warn("discarded a stale failed batch summary: the session's summary was started again, or its transcript changed, since the job claimed it", "session_id", sessionID, "cause", cause)
		return
	}
	if err != nil {
		slog.Warn("could not record the failed summary", "session_id", sessionID, "error", err)
		return
	}
	if hub != nil {
		hub.BroadcastSummaryFailed(sessionID, kept.Title, kept.Summary, kept.SummaryPreset, kept.SummaryError)
	}
}
