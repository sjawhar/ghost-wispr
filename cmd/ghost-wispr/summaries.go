package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"strings"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/llm"
	"github.com/sjawhar/ghost-wispr/internal/session"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/summary"
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
// The summarizer reads the transcript with each segment's speaker label
// (storage's CanonicalizeForSummary), as every summary does.
func resummarize(ctx context.Context, store *storage.SQLiteStore, summarizer presetSummarizer, hub summaryBroadcaster, publish func(sessionID string), sessionID, preset string) error {
	_, transcript, generation, err := store.CanonicalizeForSummary(sessionID)
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
	transcript, labeled, generation, err := store.CanonicalizeForSummary(sessionID)
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

	title, summaryText, preset, speakerNames, model, err := summarizer.SummarizeReported(ctx, sessionID, labeled)
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

// summarizeBackfillSession makes the summary of session id online, one whose
// summary is missing, failed or never ended (a start's backlog, or POST
// /api/sessions/batch-backfill gone online), and publishes it once stored
// (publish). It announces on hub, as the other summary paths do, that the
// summary runs, then the summary stored, or the title and summary the
// session keeps with why it failed (failSummary), so the page shows each
// with no reload. A session with no words gets an empty completed summary.
// Each is stored only at the transcript generation read with the text. A
// session whose summary is being made now, or was made since its backlog
// listed it (storage's StartBackfillSummary refuses it), is left to that
// summary: two backlogs that list one session summarize it once.
func summarizeBackfillSession(ctx context.Context, store *storage.SQLiteStore, summarizer session.ReportedSummarizer, hub summaryBroadcaster, publish func(sessionID string), id string) {
	log.Printf("summarizing session %s online", id)
	transcript, labeled, generation, err := store.CanonicalizeForSummary(id)
	if err != nil {
		slog.Warn("failed to read the transcript of session", "session_id", id, "error", err)
		return
	}
	if strings.TrimSpace(transcript) == "" {
		if err := store.StartEmptySummaryAtGeneration(id, generation); err != nil {
			slog.Warn("failed to start empty summary for session", "session_id", id, "error", err)
			return
		}
		_ = store.UpdateSummaryAtGeneration(id, "", "", storage.SummaryCompleted, "", "{}", "", generation)
		return
	}
	if err := store.StartBackfillSummary(id); errors.Is(err, sql.ErrNoRows) {
		log.Printf("session %s: its summary is being made now, or was made since the backlog listed it; skipping", id)
		return
	} else if err != nil {
		slog.Warn("failed to start the summary of session", "session_id", id, "error", err)
		return
	}
	hub.BroadcastSummaryReady(id, "", "", storage.SummaryRunning, "")
	title, summaryText, preset, speakerNames, model, err := summarizer.SummarizeReported(ctx, id, strings.TrimSpace(labeled))
	if err != nil {
		slog.Warn("summarization failed", "session_id", id, "error", err)
		failSummary(store, hub, id, generation, err)
		return
	}
	if err := store.UpdateSummaryAtGeneration(id, title, summaryText, storage.SummaryCompleted, preset, speakerNames, model, generation); err != nil {
		slog.Warn("discarded stale summary", "session_id", id, "error", err)
		return
	}
	log.Printf("summarized session %s with preset %s", id, preset)
	hub.BroadcastSummaryReady(id, title, summaryText, storage.SummaryCompleted, preset)
	publish(id)
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

// presetTester is the settings page's test of a summary preset: the answer
// of the preset's model, or summarization.model's, to the preset's prompt
// over session sessionID's transcript, as the config saved last has them.
// The model reads the transcript as a summary does, each segment after its
// speaker's label.
func presetTester(cfgStore interface{ Get() config.Config }, store *storage.SQLiteStore, clients summary.ClientFactory) func(ctx context.Context, presetName, sessionID string) (string, error) {
	return func(ctx context.Context, presetName, sessionID string) (string, error) {
		latestCfg := cfgStore.Get()
		preset, ok := latestCfg.Summarization.Presets[presetName]
		if !ok {
			return "", fmt.Errorf("preset %q not found", presetName)
		}

		_, transcript, _, err := store.CanonicalizeForSummary(sessionID)
		if err != nil {
			return "", err
		}

		modelStr := preset.Model
		if modelStr == "" {
			modelStr = latestCfg.Summarization.Model
		}

		provider, model, err := config.ParseModelRef(modelStr)
		if err != nil {
			return "", err
		}

		client, err := clients(provider, model)
		if err != nil {
			return "", err
		}

		userContent := strings.ReplaceAll(preset.UserTemplate, "{{transcript}}", transcript)
		msgs := []llm.Message{
			{Role: "system", Content: preset.SystemPrompt},
			{Role: "user", Content: userContent},
		}

		return client.Complete(ctx, msgs)
	}
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
