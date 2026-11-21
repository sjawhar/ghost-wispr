package storage

import (
	"strings"
	"testing"
)

// TestASummaryReadsEachSegmentUnderItsSpeaker fails if the transcript
// CanonicalizeForSummary gives a summary lacks a segment's speaker label, or
// holds other words than the canonical transcript; or if the canonical
// transcript, the one stored and the one search indexes, gains a label; or if
// the two are read at another generation than the one stored. A session
// refined before refinements kept their words (refined_revision 0) keeps live
// segments that are not the refined text: it is summarized from the refined
// text, unlabelled, not from those segments.
func TestASummaryReadsEachSegmentUnderItsSpeaker(t *testing.T) {
	const labeled = "Speaker 0: before bridge\nSpeaker 1: private deletion token\nSpeaker 0: after bridge\n"
	for _, tc := range []struct {
		name          string
		prepare       func(t *testing.T, store *SQLiteStore)
		wantCanonical string
		wantLabeled   string
	}{
		{"streaming", func(*testing.T, *SQLiteStore) {}, "before bridge\nprivate deletion token\nafter bridge\n", labeled},
		{"refined", func(t *testing.T, store *SQLiteStore) {
			publishRefinedSegmentTestSession(t, store, "s", "recording.mp3", "b")
		}, "before bridge private deletion token after bridge", labeled},
		{"refined before refinements kept their words", func(t *testing.T, store *SQLiteStore) {
			if _, err := store.DB().Exec(`UPDATE sessions SET refined_transcript = 'Refined words.', refinement_status = ? WHERE id = 's'`, RefinementCompleted); err != nil {
				t.Fatal(err)
			}
		}, "Refined words.", "Refined words."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestSQLiteStore(t)
			_, _ = segmentTestSession(t, store, "s")
			tc.prepare(t, store)

			canonical, summaryText, generation, err := store.CanonicalizeForSummary("s")
			if err != nil {
				t.Fatal(err)
			}
			if canonical != tc.wantCanonical || summaryText != tc.wantLabeled {
				t.Fatalf("CanonicalizeForSummary = canonical %q, summary text %q; want %q and %q", canonical, summaryText, tc.wantCanonical, tc.wantLabeled)
			}
			stored, _, storedGeneration, err := store.GetCanonicalTranscriptWithGeneration("s")
			if err != nil {
				t.Fatal(err)
			}
			if stored != canonical || strings.Contains(stored, "Speaker") || storedGeneration != generation {
				t.Fatalf("stored canonical transcript %q at generation %d; want %q, unlabelled, at %d", stored, storedGeneration, canonical, generation)
			}
		})
	}
}
