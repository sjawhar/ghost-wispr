package main

import (
	"context"
	"fmt"
	"log"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// presetSummarizer makes the summary of a transcript, with the preset named
// or with one it picks.
type presetSummarizer interface {
	Summarize(ctx context.Context, sessionID, transcript string) (title, summary, preset, speakerNames string, err error)
	SummarizeWithPreset(ctx context.Context, sessionID, transcript, preset string) (title, summary, speakerNames string, err error)
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

	var title string
	var summaryText string
	var presetUsed string
	var speakerNames string
	if preset != "" {
		presetUsed = preset
		title, summaryText, speakerNames, err = summarizer.SummarizeWithPreset(ctx, sessionID, transcript, preset)
	} else {
		title, summaryText, presetUsed, speakerNames, err = summarizer.Summarize(ctx, sessionID, transcript)
	}
	if err != nil {
		failSummary(store, hub, sessionID, err)
		return err
	}

	if err := store.UpdateSummary(sessionID, title, summaryText, storage.SummaryCompleted, presetUsed, speakerNames); err != nil {
		err = fmt.Errorf("store summary: %w", err)
		failSummary(store, hub, sessionID, err)
		return err
	}
	hub.BroadcastSummaryReady(sessionID, title, summaryText, storage.SummaryCompleted, presetUsed)
	publish(sessionID)
	return nil
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
