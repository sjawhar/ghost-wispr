package main

// The #65 round 2 review's findings on the check after each "Delete text and
// audio" cut: which neighbour words may explain a word heard at the cut, an
// empty transcription, and the status warning of a refused cut.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// cutRuleWords is a recording with a neighbour saying "tractor" 2 s before
// the segment that says it again, and a neighbour right after the segment.
func cutRuleWords() ([]storage.RefinedWord, transcribe.Segment) {
	word := func(ordinal int, text string, start, end float64) storage.RefinedWord {
		return storage.RefinedWord{RecordingPath: "rec.mp3", RecordingSHA256: "sha", Ordinal: ordinal, Text: text, Start: start, End: end}
	}
	words := []storage.RefinedWord{
		word(0, "My", 0.2, 0.4), word(1, "tractor.", 0.5, 0.9),
		word(2, "Orange", 3.0, 3.4), word(3, "tractor", 3.5, 3.9), word(4, "ninety.", 4.0, 4.4),
		word(5, "Then", 5.5, 5.8), word(6, "left.", 5.9, 6.2),
	}
	return words, transcribe.Segment{RecordingPath: "rec.mp3", RecordingSHA256: "sha", StartTime: 3.0, EndTime: 4.4}
}

// TestLeftoverRepeatedByANearbyNeighbourIsCaught fails if a neighbour word
// outside the listening window, and not cut into, explains away the same word
// of the deleted segment heard at the cut, as every neighbour word within
// 3.5 s did.
func TestLeftoverRepeatedByANearbyNeighbourIsCaught(t *testing.T) {
	words, selected := cutRuleWords()
	start, end := refinedSpliceBoundsWithPad(words, &selected, deletionAudioPad)
	heard := []transcribe.TimestampedWord{
		{Text: "My", Start: 0.2, End: 0.4}, {Text: "tractor.", Start: 0.5, End: 0.9},
		{Text: "tractor", Start: start - 0.1, End: start + 0.2}, // the segment's tail, left at the cut
		{Text: "Then", Start: start + 0.65, End: start + 0.95},
	}
	if got := leftoverWords(words, &selected, start, end, heard, deletionCutWindow); !slices.Equal(got, []string{"tractor"}) {
		t.Fatalf("leftover words = %q, want the segment's \"tractor\" heard at the cut, though a neighbour 2 s earlier says it too", got)
	}
}

// TestNeighbourWordInTheWindowExplainsAHeardWord fails if a neighbour word
// the listening window holds, or the pad cut into, does not explain the same
// word heard at the cut.
func TestNeighbourWordInTheWindowExplainsAHeardWord(t *testing.T) {
	words, selected := cutRuleWords()
	// The neighbour before the segment says "orange" right before the cut.
	words[1] = storage.RefinedWord{RecordingPath: "rec.mp3", RecordingSHA256: "sha", Ordinal: 1, Text: "orange", Start: 2.3, End: 2.5}
	start, end := refinedSpliceBoundsWithPad(words, &selected, deletionAudioPad)
	heard := []transcribe.TimestampedWord{{Text: "orange", Start: start - 0.3, End: start - 0.1}}
	if got := leftoverWords(words, &selected, start, end, heard, deletionCutWindow); len(got) != 0 {
		t.Fatalf("leftover words = %q, want none: the neighbour in the window says it", got)
	}
}

// emptyThenTranscription answers the check's transcriptions in turn.
type answers struct {
	heard [][]transcribe.TimestampedWord
	asked int
}

func (a *answers) transcribe(context.Context, string, string) ([]transcribe.TimestampedWord, error) {
	a.asked++
	if a.asked > len(a.heard) {
		return nil, nil
	}
	return a.heard[a.asked-1], nil
}

func cutCheckRunner(t *testing.T, a *answers) (*segmentDeleteRunner, string) {
	t.Helper()
	recording := filepath.Join(t.TempDir(), "rec.mp3")
	if err := os.WriteFile(recording, []byte("audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &segmentDeleteRunner{
		splice:        func(context.Context, string, float64, float64) (string, error) { return "cut", nil },
		transcribeCut: a.transcribe,
	}, recording
}

func filesBeside(t *testing.T, recording string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(recording))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if entry.Name() != filepath.Base(recording) {
			names = append(names, entry.Name())
		}
	}
	return names
}

// TestCutCheckRefusesAnEmptyTranscription fails if a transcription of the cut
// recording that holds no words passes the check: it heard nothing, so it
// cannot say the deleted words are gone.
func TestCutCheckRefusesAnEmptyTranscription(t *testing.T) {
	words, selected := cutRuleWords()
	a := &answers{}
	runner, recording := cutCheckRunner(t, a)
	_, _, _, _, err := runner.cutAndCheck(context.Background(), "nova-3", recording, words, &selected, -1)
	if !errors.Is(err, errDeletionCutUnchecked) {
		t.Fatalf("cut with empty transcriptions = %v, want refused as unchecked", err)
	}
	if a.asked != 2 {
		t.Fatalf("transcriptions asked = %d, want the empty one retried once", a.asked)
	}
	if left := filesBeside(t, recording); len(left) != 0 {
		t.Fatalf("a refused cut left %v beside the recording", left)
	}
}

// TestCutCheckRetriesAnEmptyTranscription fails if one empty transcription
// refuses a cut that a second transcription finds clean.
func TestCutCheckRetriesAnEmptyTranscription(t *testing.T) {
	words, selected := cutRuleWords()
	start, _ := refinedSpliceBoundsWithPad(words, &selected, deletionAudioPad)
	a := &answers{heard: [][]transcribe.TimestampedWord{nil, {{Text: "Then", Start: start + 0.65, End: start + 0.95}}}}
	runner, recording := cutCheckRunner(t, a)
	path, _, _, _, err := runner.cutAndCheck(context.Background(), "nova-3", recording, words, &selected, -1)
	if err != nil {
		t.Fatalf("cut after an empty, then a clean transcription = %v, want accepted", err)
	}
	_ = os.Remove(path)
	if a.asked != 2 {
		t.Fatalf("transcriptions asked = %d, want 2", a.asked)
	}
}

// refusedCutFixture is an ended session whose second refined segment, by
// another speaker, says "Orange tractor ninety." from 3 to 4.4 s, in an open
// store.
func refusedCutFixture(t *testing.T) (*storage.SQLiteStore, string, int64, int) {
	t.Helper()
	dbPath, recording, segmentID, revision := refusedCutDatabase(t)
	return openTestStore(t, dbPath), recording, segmentID, revision
}

// refusedCutDatabase writes refusedCutFixture's session to a database file,
// closed, and returns its path.
func refusedCutDatabase(t *testing.T) (dbPath, recording string, segmentID int64, revision int) {
	t.Helper()
	recording = filepath.Join(t.TempDir(), "recording.mp3")
	if err := os.WriteFile(recording, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	hash, err := fileSHA256(recording)
	if err != nil {
		t.Fatal(err)
	}
	dbPath = filepath.Join(t.TempDir(), "test.db")
	store, err := storage.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	if err := store.CreateSession("s", started); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("s", started.Add(time.Minute), recording); err != nil {
		t.Fatal(err)
	}
	word := func(ordinal, speaker int, text string, start, end float64) storage.RefinedWord {
		return storage.RefinedWord{RecordingPath: recording, RecordingSHA256: hash, Ordinal: ordinal, Speaker: speaker, Text: text, Start: start, End: end}
	}
	words := []storage.RefinedWord{
		word(0, 0, "So", 0.2, 0.5), word(1, 0, "we", 0.6, 0.8), word(2, 0, "met.", 0.9, 1.2),
		word(3, 1, "Orange", 63.0, 63.4), word(4, 1, "tractor", 63.5, 63.9), word(5, 1, "ninety.", 64.0, 64.4),
		word(6, 0, "Then", 66.5, 66.8), word(7, 0, "left.", 66.9, 67.2),
	}
	revision, err = store.PublishRefinementAtGeneration("s", storage.RefinedTranscript(words), "nova-3", words, storage.BuildRefinedSegments(words), 0)
	if err != nil {
		t.Fatal(err)
	}
	segments, err := store.GetSegments("s")
	if err != nil {
		t.Fatal(err)
	}
	for i := range segments {
		if strings.Contains(segments[i].Text, "tractor") {
			return dbPath, recording, segments[i].ID, revision
		}
	}
	t.Fatalf("no refined segment says tractor: %+v", segments)
	return "", "", 0, 0
}

// openTestStore opens the database at dbPath until the test ends.
func openTestStore(t *testing.T, dbPath string) *storage.SQLiteStore {
	t.Helper()
	store, err := storage.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// TestRefusedCutShowsOnTheStatusPageWithoutItsWords fails if a refused "Delete
// text and audio" does not appear among /api/status's warnings, if its warning
// quotes the words the person asked to delete rather than naming the session
// and the segment's time, or if it stays once "Delete text" removes them.
func TestRefusedCutShowsOnTheStatusPageWithoutItsWords(t *testing.T) {
	store, recording, segmentID, revision := refusedCutFixture(t)
	refused := newRefusedCuts(store)
	registry := newWarningsRegistry()
	registry.Set("other", "Another warning")
	status := apiStatusWarnings(registry, refused)
	runner := segmentDeleteRunner{
		store: store,
		ensureRecordings: func(context.Context, storage.Session) (deletionRecordingSet, error) {
			return deletionRecordingSet{Paths: []string{recording}}, nil
		},
		splice: func(context.Context, string, float64, float64) (string, error) { return "after", nil },
		transcribeCut: func(_ context.Context, _, _ string) ([]transcribe.TimestampedWord, error) {
			// Every cut still holds the segment's "tractor" at the cut point.
			return []transcribe.TimestampedWord{{Text: "So", Start: 0.2, End: 0.5}, {Text: "tractor", Start: 62.4, End: 62.7}}, nil
		},
		refused: refused,
	}
	if err := runner.DeleteRefinedAudio(context.Background(), "s", segmentID, revision); !errors.Is(err, errDeletionCutHeard) {
		t.Fatalf("delete = %v, want refused", err)
	}
	warnings := status()
	if len(warnings) != 2 || !slices.Contains(warnings, "Another warning") {
		t.Fatalf("status warnings = %q, want the registry's and the refused cut's", warnings)
	}
	got := warnings[slices.IndexFunc(warnings, func(w string) bool { return w != "Another warning" })]
	for _, want := range []string{"session s", "1:03–1:04", "recording.mp3", "Delete text"} {
		if !strings.Contains(got, want) {
			t.Errorf("refused cut warning %q does not name %q", got, want)
		}
	}
	for _, word := range []string{"Orange", "tractor", "ninety"} {
		if strings.Contains(strings.ToLower(got), strings.ToLower(word)) {
			t.Errorf("refused cut warning %q quotes the deleted word %q", got, word)
		}
	}
	if err := runner.DeleteRefinedText(context.Background(), "s", segmentID, revision); err != nil {
		t.Fatalf("Delete text: %v", err)
	}
	if warnings := status(); !slices.Equal(warnings, []string{"Another warning"}) {
		t.Fatalf("status warnings after Delete text = %q, want the refused cut's gone", warnings)
	}
}

// TestRefusedCutWarningMovesWithALaterCut fails if a successful cut earlier in
// the recording leaves a refused cut's warning naming its old time, or drops
// it, and if a cut of the refused segment itself keeps it. The cuts run
// through the deletion runner, so the store moves the refused cut in the
// splice's own transaction.
func TestRefusedCutWarningMovesWithALaterCut(t *testing.T) {
	store, recording, segmentID, revision := refusedCutFixture(t)
	refused := newRefusedCuts(store)
	heard := []transcribe.TimestampedWord{{Text: "tractor", Start: 62.4, End: 62.7}}
	runner := segmentDeleteRunner{
		store: store,
		ensureRecordings: func(context.Context, storage.Session) (deletionRecordingSet, error) {
			return deletionRecordingSet{Paths: []string{recording}}, nil
		},
		// The copy's bytes name the cut, so each cut is a new recording.
		splice: func(_ context.Context, path string, start, end float64) (string, error) {
			if err := os.WriteFile(path, fmt.Appendf(nil, "cut %.3f-%.3f of %s", start, end, path), 0o600); err != nil {
				return "", err
			}
			return fileSHA256(path)
		},
		transcribeCut: func(context.Context, string, string) ([]transcribe.TimestampedWord, error) { return heard, nil },
		refused:       refused,
	}
	if err := runner.DeleteRefinedAudio(context.Background(), "s", segmentID, revision); !errors.Is(err, errDeletionCutHeard) {
		t.Fatalf("delete = %v, want refused", err)
	}
	segmentSaying := func(text string) (int64, int) {
		t.Helper()
		session, err := store.GetSession("s")
		if err != nil {
			t.Fatal(err)
		}
		segments, err := store.GetSegments("s")
		if err != nil {
			t.Fatal(err)
		}
		for i := range segments {
			if strings.Contains(segments[i].Text, text) {
				return segments[i].ID, session.RefinedRevision
			}
		}
		t.Fatalf("no segment says %q: %+v", text, segments)
		return 0, 0
	}

	// A clean cut of "So we met." takes 0 to 1.65 s out of the recording.
	heard = []transcribe.TimestampedWord{{Text: "Then", Start: 70, End: 70.3}}
	first, revision := segmentSaying("So we met.")
	if err := runner.DeleteRefinedAudio(context.Background(), "s", first, revision); err != nil {
		t.Fatalf("clean cut before the refused segment: %v", err)
	}
	if got := refused.Warnings(); len(got) != 1 || !strings.Contains(got[0], "1:01–1:02") {
		t.Fatalf("warnings after a cut before the segment = %q, want one with its time moved back 1.65 s", got)
	}

	tractor, revision := segmentSaying("tractor")
	if err := runner.DeleteRefinedAudio(context.Background(), "s", tractor, revision); err != nil {
		t.Fatalf("clean cut of the refused segment: %v", err)
	}
	if got := refused.Warnings(); len(got) != 0 {
		t.Fatalf("warnings after the segment was cut = %q, want none", got)
	}
}
