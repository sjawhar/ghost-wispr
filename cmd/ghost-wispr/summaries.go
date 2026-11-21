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

	"github.com/sjawhar/ghost-wispr/internal/session"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// presetSummarizer makes the summary of a transcript, with the preset named
// or with one it picks.
type presetSummarizer interface {
	Summarize(ctx context.Context, sessionID, transcript string) (title, summary, preset, speakerNames string, err error)
	SummarizeWithPreset(ctx context.Context, sessionID, transcript, preset string) (title, summary, speakerNames string, err error)
}

type reportedPresetSummarizer interface {
	SummarizeWithPresetReported(ctx context.Context, sessionID, transcript, preset string) (title, summary, speakerNames, model string, err error)
}

// summaryBroadcaster announces the progress of a session's summary.
type summaryBroadcaster interface {
	BroadcastSummaryReady(sessionID, title, summary, status, preset string)
	// BroadcastSummaryFailed announces a failed summary, why it failed
	// (reason), and the title, summary and preset the session keeps.
	BroadcastSummaryFailed(sessionID, title, summary, preset, reason string)
}

// resummarize makes session sessionID's summary again, with preset if one is
// named and with one the summarizer picks if not, and publishes it once
// stored (publish). While it runs, and if it fails, the session keeps its
// title and its last good summary (failSummary). The summary is stored only
// while the session has the transcript generation read with the text it
// summarizes, so a summary of the text before a segment deletion is refused.
func resummarize(ctx context.Context, store *storage.SQLiteStore, summarizer presetSummarizer, hub summaryBroadcaster, publish func(sessionID string), sessionID, preset string) error {
	transcript, generation, err := store.Canonicalize(sessionID)
	if err != nil {
		return err
	}

	_ = store.StartSummary(sessionID)
	hub.BroadcastSummaryReady(sessionID, "", "", storage.SummaryRunning, "")

	var title, summaryText, presetUsed, speakerNames, model string
	if preset != "" {
		presetUsed = preset
		if reporter, ok := summarizer.(reportedPresetSummarizer); ok {
			title, summaryText, speakerNames, model, err = reporter.SummarizeWithPresetReported(ctx, sessionID, transcript, preset)
		} else {
			title, summaryText, speakerNames, err = summarizer.SummarizeWithPreset(ctx, sessionID, transcript, preset)
		}
	} else if reporter, ok := summarizer.(session.ReportedSummarizer); ok {
		title, summaryText, presetUsed, speakerNames, model, err = reporter.SummarizeReported(ctx, sessionID, transcript)
	} else {
		title, summaryText, presetUsed, speakerNames, err = summarizer.Summarize(ctx, sessionID, transcript)
	}
	if err != nil {
		if cutOffByTheStop(ctx) {
			return err
		}
		failSummary(store, hub, sessionID, generation, err)
		return err
	}

	if err := store.UpdateSummaryAtGeneration(sessionID, title, summaryText, storage.SummaryCompleted, presetUsed, speakerNames, model, generation); err != nil {
		err = fmt.Errorf("store summary: %w", err)
		if errors.Is(err, sql.ErrNoRows) {
			// The text changed since it was read; a newer run owns the
			// summary, so this one records no failure.
			slog.Warn("discarded stale summary", "session_id", sessionID, "error", err)
			return err
		}
		failSummary(store, hub, sessionID, generation, err)
		return err
	}
	hub.BroadcastSummaryReady(sessionID, title, summaryText, storage.SummaryCompleted, presetUsed)
	publish(sessionID)
	return nil
}

// summarizeMergedSession makes the summary of sessionID, a session a merge
// just made, and publishes it once stored (publish). A merged session with no
// words gets an empty completed summary. Each is stored only at the
// transcript generation read with the text.
func summarizeMergedSession(ctx context.Context, store *storage.SQLiteStore, summarizer session.ReportedSummarizer, hub summaryBroadcaster, publish func(sessionID string), sessionID string) {
	transcript, generation, err := store.Canonicalize(sessionID)
	if err != nil {
		slog.Warn("failed to read the transcript of merged session", "session_id", sessionID, "error", err)
		return
	}
	if strings.TrimSpace(transcript) == "" {
		if err := store.StartEmptySummaryAtGeneration(sessionID, generation); err != nil {
			slog.Warn("failed to start empty summary for merged session", "session_id", sessionID, "error", err)
			return
		}
		_ = store.UpdateSummaryAtGeneration(sessionID, "", "", storage.SummaryCompleted, "", "{}", "", generation)
		return
	}

	_ = store.StartSummary(sessionID)
	hub.BroadcastSummaryReady(sessionID, "", "", storage.SummaryRunning, "")

	title, summaryText, preset, speakerNames, model, err := summarizer.SummarizeReported(ctx, sessionID, transcript)
	if err != nil {
		slog.Warn("summarization failed for merged session", "session_id", sessionID, "error", err)
		failSummary(store, hub, sessionID, generation, err)
		return
	}

	if err := store.UpdateSummaryAtGeneration(sessionID, title, summaryText, storage.SummaryCompleted, preset, speakerNames, model, generation); err != nil {
		slog.Warn("discarded stale merged summary", "session_id", sessionID, "error", err)
		return
	}
	hub.BroadcastSummaryReady(sessionID, title, summaryText, storage.SummaryCompleted, preset)
	publish(sessionID)
}

// summarizeBackfillSession makes, at startup, the summary of session id, one
// whose summary is missing, failed or never ended, and publishes it once
// stored (publish). A session with no words gets an empty completed summary.
// Each is stored only at the transcript generation read with the text.
func summarizeBackfillSession(ctx context.Context, store *storage.SQLiteStore, summarizer session.ReportedSummarizer, publish func(sessionID string), id string) {
	log.Printf("startup: summarizing session %s synchronously", id)
	transcript, generation, err := store.Canonicalize(id)
	if err != nil {
		slog.Warn("failed to read the transcript of session", "session_id", id, "error", err)
		return
	}
	transcript = strings.TrimSpace(transcript)
	if transcript == "" {
		if err := store.StartEmptySummaryAtGeneration(id, generation); err != nil {
			slog.Warn("failed to start empty summary for session", "session_id", id, "error", err)
			return
		}
		_ = store.UpdateSummaryAtGeneration(id, "", "", storage.SummaryCompleted, "", "{}", "", generation)
		return
	}
	_ = store.StartSummary(id)
	title, summaryText, preset, speakerNames, model, err := summarizer.SummarizeReported(ctx, id, transcript)
	if err != nil {
		if cutOffByTheStop(ctx) {
			return
		}
		slog.Warn("summarization failed", "session_id", id, "error", err)
		failSummary(store, nil, id, generation, err)
		return
	}
	if err := store.UpdateSummaryAtGeneration(id, title, summaryText, storage.SummaryCompleted, preset, speakerNames, model, generation); err != nil {
		slog.Warn("discarded stale summary", "session_id", id, "error", err)
		return
	}
	log.Printf("summarized session %s with preset %s", id, preset)
	publish(id)
}

// cutOffByTheStop reports whether the stop cut off the work ctx is for
// (session.ErrStopped). A summary cut off so is left running, with the
// session's last good one kept, and the next start's backfill makes it
// (storage.SQLiteStore.GetSessionsForBackfill): it did not fail.
func cutOffByTheStop(ctx context.Context) bool {
	if errors.Is(context.Cause(ctx), session.ErrStopped) {
		slog.Info("summary cut off by the stop; the next start makes it")
		return true
	}
	return false
}

// startSummaryBackfill runs the start's summary backfill (summaryBackfill.run)
// in the background, as work the stop cuts off and waits for
// (session.Manager.Run), so the store closes under none of it.
func startSummaryBackfill(ctx context.Context, manager *session.Manager, backfill summaryBackfill) {
	go func() {
		_ = manager.Run(ctx, func(ctx context.Context) error {
			backfill.run(ctx)
			return nil
		})
	}()
}

// summaryBackfill is the start's summary backfill: the sessions whose summary
// is missing, failed or never ended (storage.SQLiteStore.GetSessionsForBackfill)
// are summarized in one batch job when there are four or more and the job can
// be made (submit, then poll every pollEvery until it ends), and else one by
// one (summarizeBackfillSession).
type summaryBackfill struct {
	store      *storage.SQLiteStore
	summarizer session.ReportedSummarizer
	publish    func(sessionID string)
	submit     func(ctx context.Context, sessionIDs []string) (batchName string, count int, err error)
	poll       func(ctx context.Context, batchName string) (map[string]any, error)
	pollEvery  time.Duration
}

func (b summaryBackfill) run(ctx context.Context) {
	pendingIDs, err := b.store.GetSessionsForBackfill()
	if err != nil {
		slog.Warn("failed to query sessions needing summary", "error", err)
		return
	}
	if len(pendingIDs) == 0 {
		return
	}
	if len(pendingIDs) >= 4 {
		batchName, count, err := b.submit(ctx, pendingIDs)
		if err == nil {
			log.Printf("startup: submitting %d sessions to batch summarization", count)
			b.pollUntilDone(ctx, batchName)
			return
		}
		slog.Warn("startup batch submission failed; falling back to synchronous summarization", "sessions", len(pendingIDs), "error", err)
	}
	for _, id := range pendingIDs {
		if ctx.Err() != nil {
			return
		}
		summarizeBackfillSession(ctx, b.store, b.summarizer, b.publish, id)
	}
}

// pollUntilDone polls the batch job until it ends, or ctx does.
func (b summaryBackfill) pollUntilDone(ctx context.Context, batchName string) {
	ticker := time.NewTicker(b.pollEvery)
	defer ticker.Stop()
	for {
		result, err := b.poll(ctx, batchName)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Warn("startup batch job poll failed", "batch", batchName, "error", err)
		} else if state, _ := result["state"].(string); state != "" {
			succeeded, _ := result["succeeded"].(int)
			failed, _ := result["failed"].(int)
			switch state {
			case string(genai.JobStateSucceeded), string(genai.JobStatePartiallySucceeded):
				log.Printf("startup: batch job %s complete: %d succeeded, %d failed", batchName, succeeded, failed)
				return
			case string(genai.JobStateFailed):
				if errMsg, _ := result["error"].(string); errMsg != "" {
					log.Printf("startup: batch job %s failed: %s", batchName, errMsg)
				} else {
					log.Printf("startup: batch job %s failed", batchName)
				}
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

// resummarizeUntilStop is the API's resummarize (resummarize), run as work the
// stop cuts off and waits for (session.Manager.Run).
func resummarizeUntilStop(manager *session.Manager, store *storage.SQLiteStore, summarizer presetSummarizer, hub summaryBroadcaster, publish func(sessionID string)) func(ctx context.Context, sessionID, preset string) error {
	return func(ctx context.Context, sessionID, preset string) error {
		return manager.Run(ctx, func(ctx context.Context) error {
			return resummarize(ctx, store, summarizer, hub, publish, sessionID, preset)
		})
	}
}

// loadSessionTranscript returns session sessionID's canonical transcript, or,
// when it has none stored, the one its segments and refinement make. It
// reports false when it could not read the session.
func loadSessionTranscript(store *storage.SQLiteStore, sessionID string) (string, bool) {
	sess, err := store.GetSession(sessionID)
	if err != nil {
		slog.Warn("failed to get session", "session_id", sessionID, "error", err)
		return "", false
	}
	transcript := strings.TrimSpace(sess.CanonicalTranscript)
	if transcript != "" {
		return transcript, true
	}
	segments, err := store.GetSegments(sessionID)
	if err != nil {
		slog.Warn("failed to get segments", "session_id", sessionID, "error", err)
		return "", false
	}
	return strings.TrimSpace(buildCanonicalTranscript(store, sessionID, segments)), true
}

// summaryRepairStore is what repairing stored summaries reads and writes.
type summaryRepairStore interface {
	GetDates() ([]string, error)
	GetSessionsByDate(date string, includeDiscarded bool) ([]storage.Session, error)
	GetSessionWithGeneration(id string) (storage.Session, int, error)
	UpdateSummaryAtGeneration(sessionID, title, summary, status, preset, speakerNames, model string, generation int) error
}

// repairStructuredSummaries rewrites every stored summary that is still the
// raw structured (JSON) reply of a summarizer as its title, summary text and
// speaker names, publishes each one that changed (publish), and returns how
// many it rewrote. It reads each summary with its transcript generation and
// rewrites it only at that generation, so it skips a summary a segment
// deletion cleared since: it never writes the deleted words back.
func repairStructuredSummaries(ctx context.Context, store summaryRepairStore, publish func(sessionID string)) (int, error) {
	dates, err := store.GetDates()
	if err != nil {
		return 0, err
	}

	repaired := 0
	seenSessionIDs := make(map[string]struct{})
	for _, date := range dates {
		select {
		case <-ctx.Done():
			return repaired, ctx.Err()
		default:
		}

		sessions, err := store.GetSessionsByDate(date, true)
		if err != nil {
			return repaired, err
		}

		for i := range sessions {
			if _, exists := seenSessionIDs[sessions[i].ID]; exists {
				continue
			}
			seenSessionIDs[sessions[i].ID] = struct{}{}

			if !strings.HasPrefix(strings.TrimSpace(sessions[i].Summary), "{") {
				continue
			}
			sess, generation, err := store.GetSessionWithGeneration(sessions[i].ID)
			if err != nil {
				return repaired, err
			}
			trimmedSummary := strings.TrimSpace(sess.Summary)
			if !strings.HasPrefix(trimmedSummary, "{") {
				continue
			}

			title, summaryText, speakerNames, err := parseStoredStructuredSummary(trimmedSummary)
			if err != nil {
				continue
			}
			if title == "" {
				title = sess.Title
			}
			if speakerNames == "" {
				speakerNames = "{}"
			}

			if err := store.UpdateSummaryAtGeneration(sess.ID, title, summaryText, sess.SummaryStatus, sess.SummaryPreset, speakerNames, sess.SummaryModel, generation); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					slog.Warn("discarded the repair of a session's summary: its transcript changed since it was read", "session_id", sess.ID)
					continue
				}
				return repaired, err
			}
			if title != sess.Title || summaryText != sess.Summary || speakerNames != sess.SpeakerNames {
				publish(sess.ID)
			}
			repaired++
		}
	}

	return repaired, nil
}

// failSummary records that making session sessionID's summary from the text
// of transcript generation generation failed, and why (cause), and announces
// on hub (when not nil) the title and summary the session keeps, its last
// good ones if it has them, with the reason. A failed summary takes nothing
// away. Once the text has changed since (a segment deletion), it records
// nothing: the newer run's status stays.
func failSummary(store *storage.SQLiteStore, hub summaryBroadcaster, sessionID string, generation int, cause error) {
	kept, err := store.FailSummaryAtGeneration(sessionID, fmt.Sprintf("summarization failed: %v", cause), generation)
	if errors.Is(err, sql.ErrNoRows) {
		slog.Warn("discarded a stale failed summary: its transcript changed since it was read", "session_id", sessionID, "cause", cause)
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
