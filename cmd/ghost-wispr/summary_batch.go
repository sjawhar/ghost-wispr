package main

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/summary"
)

const batchResultDiscarded = "discarded"

// readBatchSummarySessions reads every submitted transcript and deletion
// generation from the same SQLite row, so a delete cannot pair old text with a
// newer generation. Sessions whose transcript is empty are not submitted:
// emptyTranscripts maps each to the generation read with its transcript.
func readBatchSummarySessions(store *storage.SQLiteStore, sessionIDs []string) (sessions []summary.BatchSession, generations, emptyTranscripts map[string]int, err error) {
	sessions = make([]summary.BatchSession, 0, len(sessionIDs))
	generations = make(map[string]int, len(sessionIDs))
	emptyTranscripts = make(map[string]int)
	for _, sessionID := range sessionIDs {
		transcript, generation, err := store.Canonicalize(sessionID)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("canonicalize batch summary input for %s: %w", sessionID, err)
		}
		if strings.TrimSpace(transcript) == "" {
			emptyTranscripts[sessionID] = generation
			continue
		}
		sessions = append(sessions, summary.BatchSession{ID: sessionID, Transcript: transcript})
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
		_ = store.UpdateSummaryAtGeneration(id, "", "", storage.SummaryCompleted, "", "{}", "", generation)
	}
}

// applySummaryBatchResults stores each terminal response only at its submitted
// generation. Missing responses are failures; already-handled sessions are
// skipped so a repeated poll cannot publish a result twice.
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
			if err := failBatchSummary(store, hub, batchName, result.SessionID, batchSession.Generation, result.Error); err != nil {
				return succeeded, failed, err
			}
			failed++
			continue
		}
		if err := store.UpdateSummaryAtGeneration(result.SessionID, result.Title, result.Summary, storage.SummaryCompleted, result.PresetName, result.SpeakerNames, result.Model, batchSession.Generation); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				if err := store.MarkSummaryBatchSessionDone(batchName, result.SessionID, batchResultDiscarded); err != nil {
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
		if err := failBatchSummary(store, hub, batchName, sessionID, batchSession.Generation, errors.New("batch job returned no result for session")); err != nil {
			return succeeded, failed, err
		}
		failed++
	}
	return succeeded, failed, nil
}

func failBatchSummary(store *storage.SQLiteStore, hub summaryBroadcaster, batchName, sessionID string, generation int, cause error) error {
	kept, err := store.FailSummaryAtGeneration(sessionID, fmt.Sprintf("summarization failed: %v", cause), generation)
	if errors.Is(err, sql.ErrNoRows) {
		return store.MarkSummaryBatchSessionDone(batchName, sessionID, batchResultDiscarded)
	}
	if err != nil {
		return err
	}
	if err := store.MarkSummaryBatchSessionDone(batchName, sessionID, storage.SummaryFailed); err != nil {
		return err
	}
	if hub != nil {
		hub.BroadcastSummaryReady(sessionID, kept.Title, kept.Summary, storage.SummaryFailed, kept.SummaryPreset)
	}
	return nil
}
