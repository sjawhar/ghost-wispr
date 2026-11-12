package main

import (
	"context"
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
// title and its last good summary (failSummary).
func resummarize(ctx context.Context, store *storage.SQLiteStore, summarizer presetSummarizer, hub summaryBroadcaster, publish func(sessionID string), sessionID, preset string) error {
	segments, err := store.GetSegments(sessionID)
	if err != nil {
		return err
	}

	transcript := buildCanonicalTranscript(store, sessionID, segments)

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
		failSummary(store, hub, sessionID, err)
		return err
	}

	if err := store.UpdateSummaryWithModel(sessionID, title, summaryText, storage.SummaryCompleted, presetUsed, speakerNames, model); err != nil {
		err = fmt.Errorf("store summary: %w", err)
		failSummary(store, hub, sessionID, err)
		return err
	}
	hub.BroadcastSummaryReady(sessionID, title, summaryText, storage.SummaryCompleted, presetUsed)
	publish(sessionID)
	return nil
}

// summarizeMergedSession makes the summary of sessionID, a session a merge
// just made, and publishes it once stored (publish). A merged session with no
// words gets an empty completed summary.
func summarizeMergedSession(ctx context.Context, store *storage.SQLiteStore, summarizer session.ReportedSummarizer, hub summaryBroadcaster, publish func(sessionID string), sessionID string) {
	segments, err := store.GetSegments(sessionID)
	if err != nil {
		log.Printf("warning: failed to get segments for merged session %s: %v", sessionID, err)
		return
	}

	transcript := buildCanonicalTranscript(store, sessionID, segments)
	if transcript == "" {
		_ = store.UpdateSummary(sessionID, "", "", storage.SummaryCompleted, "", "{}")
		return
	}

	_ = store.StartSummary(sessionID)
	hub.BroadcastSummaryReady(sessionID, "", "", storage.SummaryRunning, "")

	title, summaryText, preset, speakerNames, model, err := summarizer.SummarizeReported(ctx, sessionID, transcript)
	if err != nil {
		log.Printf("warning: summarization failed for merged session %s: %v", sessionID, err)
		failSummary(store, hub, sessionID, err)
		return
	}

	if err := store.UpdateSummaryWithModel(sessionID, title, summaryText, storage.SummaryCompleted, preset, speakerNames, model); err != nil {
		log.Printf("warning: discarded stale merged summary for session %s: %v", sessionID, err)
		return
	}
	hub.BroadcastSummaryReady(sessionID, title, summaryText, storage.SummaryCompleted, preset)
	publish(sessionID)
}

// summarizeBackfillSession makes, at startup, the summary of session id, one
// whose summary is missing, failed or never ended, and publishes it once
// stored (publish). A session with no words gets an empty completed summary.
func summarizeBackfillSession(ctx context.Context, store *storage.SQLiteStore, summarizer session.ReportedSummarizer, publish func(sessionID string), id string) {
	log.Printf("startup: summarizing session %s synchronously", id)
	transcript, ok := loadSessionTranscript(store, id)
	if !ok {
		return
	}
	if transcript == "" {
		_ = store.UpdateSummary(id, "", "", storage.SummaryCompleted, "", "{}")
		return
	}
	_ = store.StartSummary(id)
	title, summaryText, preset, speakerNames, model, err := summarizer.SummarizeReported(ctx, id, transcript)
	if err != nil {
		log.Printf("warning: summarization failed for %s: %v", id, err)
		failSummary(store, nil, id, err)
		return
	}
	_ = store.UpdateSummaryWithModel(id, title, summaryText, storage.SummaryCompleted, preset, speakerNames, model)
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

// repairStructuredSummaries rewrites every stored summary that is still the
// raw structured (JSON) reply of a summarizer as its title, summary text and
// speaker names, publishes each one that changed (publish), and returns how
// many it rewrote.
func repairStructuredSummaries(ctx context.Context, store *storage.SQLiteStore, publish func(sessionID string)) (int, error) {
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

			trimmedSummary := strings.TrimSpace(sessions[i].Summary)
			if !strings.HasPrefix(trimmedSummary, "{") {
				continue
			}

			title, summaryText, speakerNames, err := parseStoredStructuredSummary(trimmedSummary)
			if err != nil {
				continue
			}
			if title == "" {
				title = sessions[i].Title
			}
			if speakerNames == "" {
				speakerNames = "{}"
			}

			if err := store.UpdateSummaryWithModel(sessions[i].ID, title, summaryText, sessions[i].SummaryStatus, sessions[i].SummaryPreset, speakerNames, sessions[i].SummaryModel); err != nil {
				return repaired, err
			}
			if title != sessions[i].Title || summaryText != sessions[i].Summary || speakerNames != sessions[i].SpeakerNames {
				publish(sessions[i].ID)
			}
			repaired++
		}
	}

	return repaired, nil
}

// failSummary records that making session sessionID's summary failed, and
// why (cause), and announces on hub (when not nil) the title and summary the
// session keeps: its last good ones, if it has them. A failed summary takes
// nothing away.
func failSummary(store *storage.SQLiteStore, hub summaryBroadcaster, sessionID string, cause error) {
	kept, err := store.FailSummary(sessionID, fmt.Sprintf("summarization failed: %v", cause))
	if err != nil {
		log.Printf("warning: could not record the failed summary of %s: %v", sessionID, err)
		return
	}
	if hub != nil {
		hub.BroadcastSummaryReady(sessionID, kept.Title, kept.Summary, storage.SummaryFailed, kept.SummaryPreset)
	}
}
