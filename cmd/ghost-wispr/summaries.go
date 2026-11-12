package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"

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
		failSummary(store, hub, sessionID, generation, err)
		return err
	}

	if err := store.UpdateSummaryAtGeneration(sessionID, title, summaryText, storage.SummaryCompleted, presetUsed, speakerNames, model, generation); err != nil {
		err = fmt.Errorf("store summary: %w", err)
		if errors.Is(err, sql.ErrNoRows) {
			// The text changed since it was read; a newer run owns the
			// summary, so this one records no failure.
			log.Printf("warning: discarded stale summary for session %s: %v", sessionID, err)
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
		log.Printf("warning: failed to read the transcript of merged session %s: %v", sessionID, err)
		return
	}
	if strings.TrimSpace(transcript) == "" {
		_ = store.UpdateSummaryAtGeneration(sessionID, "", "", storage.SummaryCompleted, "", "{}", "", generation)
		return
	}

	_ = store.StartSummary(sessionID)
	hub.BroadcastSummaryReady(sessionID, "", "", storage.SummaryRunning, "")

	title, summaryText, preset, speakerNames, model, err := summarizer.SummarizeReported(ctx, sessionID, transcript)
	if err != nil {
		log.Printf("warning: summarization failed for merged session %s: %v", sessionID, err)
		failSummary(store, hub, sessionID, generation, err)
		return
	}

	if err := store.UpdateSummaryAtGeneration(sessionID, title, summaryText, storage.SummaryCompleted, preset, speakerNames, model, generation); err != nil {
		log.Printf("warning: discarded stale merged summary for session %s: %v", sessionID, err)
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
		log.Printf("warning: failed to read the transcript of session %s: %v", id, err)
		return
	}
	transcript = strings.TrimSpace(transcript)
	if transcript == "" {
		_ = store.UpdateSummaryAtGeneration(id, "", "", storage.SummaryCompleted, "", "{}", "", generation)
		return
	}
	_ = store.StartSummary(id)
	title, summaryText, preset, speakerNames, model, err := summarizer.SummarizeReported(ctx, id, transcript)
	if err != nil {
		log.Printf("warning: summarization failed for %s: %v", id, err)
		failSummary(store, nil, id, generation, err)
		return
	}
	if err := store.UpdateSummaryAtGeneration(id, title, summaryText, storage.SummaryCompleted, preset, speakerNames, model, generation); err != nil {
		log.Printf("warning: discarded stale summary for session %s: %v", id, err)
		return
	}
	log.Printf("summarized session %s with preset %s", id, preset)
	publish(id)
}

// loadSessionTranscript returns session sessionID's canonical transcript, or,
// when it has none stored, the one its segments and refinement make. It
// reports false when it could not read the session.
func loadSessionTranscript(store *storage.SQLiteStore, sessionID string) (string, bool) {
	sess, err := store.GetSession(sessionID)
	if err != nil {
		log.Printf("warning: failed to get session %s: %v", sessionID, err)
		return "", false
	}
	transcript := strings.TrimSpace(sess.CanonicalTranscript)
	if transcript != "" {
		return transcript, true
	}
	segments, err := store.GetSegments(sessionID)
	if err != nil {
		log.Printf("warning: failed to get segments for %s: %v", sessionID, err)
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
					log.Printf("warning: discarded the repair of session %s's summary: its transcript changed since it was read", sess.ID)
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
// on hub (when not nil) the title and summary the session keeps: its last
// good ones, if it has them. A failed summary takes nothing away. Once the
// text has changed since (a segment deletion), it records nothing: the newer
// run's status stays.
func failSummary(store *storage.SQLiteStore, hub summaryBroadcaster, sessionID string, generation int, cause error) {
	kept, err := store.FailSummaryAtGeneration(sessionID, fmt.Sprintf("summarization failed: %v", cause), generation)
	if errors.Is(err, sql.ErrNoRows) {
		log.Printf("warning: discarded a stale failed summary of %s: its transcript changed since it was read: %v", sessionID, cause)
		return
	}
	if err != nil {
		log.Printf("warning: could not record the failed summary of %s: %v", sessionID, err)
		return
	}
	if hub != nil {
		hub.BroadcastSummaryReady(sessionID, kept.Title, kept.Summary, storage.SummaryFailed, kept.SummaryPreset)
	}
}
