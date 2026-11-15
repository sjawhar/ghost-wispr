package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"unicode"

	recordingname "github.com/sjawhar/ghost-wispr/internal/recording"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// deletionAudioPad widens the cut of "Delete text and audio" past the
// deleted segment's first and last word times on each side, also into
// touching neighbours: Deepgram's word times are tighter than the spoken
// words, so a cut at them leaves an onset or a tail a transcription still
// hears. Under the consent rule the deleted words must be gone, and a sliver
// of neighbouring speech is the cost (the INFRA-1027 coordinator's decision
// on the #65 review). Measured with TestMeasureDeletionAudioPad on the
// review's two recordings, 30 cuts each, by the check's rule
// (testdata/deletion_cut): with no pad 9 and 6 cuts left a deleted word heard
// next to the cut; with 0.45 s, 1 and 2, for 0.66 and 0.72 s of neighbouring
// speech per cut on average and at most 0.90 s
// (TestDeletionCutFixturesBackThePadComment checks these). The check after
// each cut handles what the pad leaves.
const deletionAudioPad = 0.45

// refinedSpliceBoundsWithPad cuts pad seconds past the segment's words on
// each side, and keeps the earlier 0.25 s margin where it cuts more: up to
// 0.25 s into the silence before a neighbouring word.
func refinedSpliceBoundsWithPad(words []storage.RefinedWord, selected *transcribe.Segment, pad float64) (float64, float64) {
	const margin = 0.25
	start, end := selected.StartTime-margin, selected.EndTime+margin
	for _, word := range words {
		if word.RecordingPath != selected.RecordingPath || word.RecordingSHA256 != selected.RecordingSHA256 {
			continue
		}
		if word.End <= selected.StartTime && word.End > start {
			start = word.End
		}
		if word.Start >= selected.EndTime && word.Start < end {
			end = word.Start
		}
	}
	start = min(start, selected.StartTime-pad)
	end = max(end, selected.EndTime+pad)
	if start < 0 {
		start = 0
	}
	return start, end
}

// deletionCutPads are the pads a cut is tried with, in order: the check after
// each cut widens to the next when it still hears a deleted word, and refuses
// past the last.
var deletionCutPads = []float64{deletionAudioPad, 0.6, 0.8}

// deletionCutWindow is how near the cut point, in the cut recording, the check
// counts a deleted word it hears: further away, a word the segment shares
// with its neighbours is most likely theirs. Measured with
// TestMeasureDeletionAudioPad.
const deletionCutWindow = 0.5

// errDeletionCutHeard refuses a "Delete text and audio" whose every cut
// still left a deleted word heard next to it. It names no word: they are the
// ones the person asked to delete.
var errDeletionCutHeard = errors.New("part of the deleted segment is still heard next to the cut")

// errDeletionCutUnchecked refuses a "Delete text and audio" whose check at the
// widest pad could not hear the cut, twice: its transcription of the cut
// recording held no words, or none within the listening window when the
// recording has neighbour words there. A check that heard nothing where the
// deleted words would be cannot say they are gone.
var errDeletionCutUnchecked = errors.New("a transcription of the cut recording heard nothing next to the cut, so the cut could not be checked")

// cutAndCheck cuts the selected segment out of a copy of recording, with each
// of deletionCutPads in turn, until a transcription of the copy hears the cut
// and no word of the segment next to it. A cut with a word heard, or with a
// check that could not hear the cut, is widened to the next pad; past the
// widest the deletion is refused, with that cut's reason. It returns the
// accepted copy, its bounds and its hash; it removes every copy it does not
// return. duration < 0 is unknown.
func (d *segmentDeleteRunner) cutAndCheck(ctx context.Context, model, recording string, words []storage.RefinedWord, selected *transcribe.Segment, duration float64) (string, float64, float64, string, error) {
	// refusal is why the last cut tried was not accepted: a word of the
	// segment heard next to it, or a check that could not hear the cut. Either
	// widens the cut to the next pad; only the widest pad's refuses.
	refusal := errDeletionCutHeard
	for _, pad := range deletionCutPads {
		start, end := refinedSpliceBoundsWithPad(words, selected, pad)
		if duration >= 0 && end > duration {
			end = duration
		}
		if end <= start {
			return "", 0, 0, "", fmt.Errorf("stored word span for segment %d has no removable audio", selected.ID)
		}
		tempPath, err := copyForSplice(recording)
		if err != nil {
			return "", 0, 0, "", err
		}
		afterHash, err := d.splice(ctx, tempPath, start, end)
		if err != nil {
			_ = os.Remove(tempPath)
			return "", 0, 0, "", fmt.Errorf("splice recording %s: %w", recording, err)
		}
		if afterHash == "" {
			if afterHash, err = recordingname.SHA256(tempPath); err != nil {
				_ = os.Remove(tempPath)
				return "", 0, 0, "", fmt.Errorf("hash spliced recording %s: %w", tempPath, err)
			}
		}
		heard, err := d.transcribeCutHearingTheCut(ctx, model, tempPath, func(heard []transcribe.TimestampedWord) bool {
			return hearsTheCut(words, selected, start, end, heard, deletionCutWindow)
		})
		switch {
		case errors.Is(err, errDeletionCutUnchecked):
			refusal = errDeletionCutUnchecked
		case err != nil:
			_ = os.Remove(tempPath)
			return "", 0, 0, "", fmt.Errorf("check the cut recording %s: %w", recording, err)
		case len(leftoverWords(words, selected, start, end, heard, deletionCutWindow)) == 0:
			return tempPath, start, end, afterHash, nil
		default:
			refusal = errDeletionCutHeard
		}
		_ = os.Remove(tempPath)
	}
	return "", 0, 0, "", fmt.Errorf("%w, even cutting %.2f s past its words on each side; the recording is unchanged",
		refusal, deletionCutPads[len(deletionCutPads)-1])
}

// transcribeCutHearingTheCut transcribes a cut recording, once more when the
// first transcription does not hear the cut, and fails with
// errDeletionCutUnchecked when neither does.
func (d *segmentDeleteRunner) transcribeCutHearingTheCut(ctx context.Context, model, path string, hears func([]transcribe.TimestampedWord) bool) ([]transcribe.TimestampedWord, error) {
	for range 2 {
		heard, err := d.transcribeCut(ctx, model, path)
		if err != nil {
			return nil, err
		}
		if hears(heard) {
			return heard, nil
		}
	}
	return nil, errDeletionCutUnchecked
}

// cutWords counts, by spoken form, the selected segment's words and its
// neighbours' words the listening window can hold: within window of the
// cut's edges, or cut into or out by the pad. A neighbour further away does
// not count.
func cutWords(words []storage.RefinedWord, selected *transcribe.Segment, start, end, window float64) (segment, near map[string]int) {
	segment, near = map[string]int{}, map[string]int{}
	for _, word := range words {
		if word.RecordingPath != selected.RecordingPath || word.RecordingSHA256 != selected.RecordingSHA256 {
			continue
		}
		text := spokenWord(word.Text)
		switch {
		case text == "":
		case word.Start >= selected.StartTime && word.End <= selected.EndTime:
			segment[text]++
		case word.End > start-window && word.Start < end+window:
			near[text]++
		}
	}
	return segment, near
}

// heardAtTheCut counts, by spoken form, the words heard, a transcription of
// the recording cut from start, holds within window seconds of the cut point.
func heardAtTheCut(heard []transcribe.TimestampedWord, start, window float64) map[string]int {
	around := map[string]int{}
	for _, word := range heard {
		if word.End >= start-window && word.Start <= start+window {
			if text := spokenWord(word.Text); text != "" {
				around[text]++
			}
		}
	}
	return around
}

// hearsTheCut reports whether heard, a transcription of the recording cut
// from start to end, can check the cut: it holds words, and when the
// recording has neighbour words the listening window can hold and the cut
// leaves at least partly in place, it heard a word within window of the cut
// point. A first or last segment, or one with silence around it, has no such
// neighbour, and then any words will do. Any word at the cut counts, not
// only the neighbours' own: on the review's recordings Deepgram heard a
// neighbour word the pad cut into as another word ("she goes" as "no") in 1
// of 60 clean cuts at 0.45 s, and requiring the neighbours' words would have
// refused it; any word at the cut flagged none of the 60.
func hearsTheCut(words []storage.RefinedWord, selected *transcribe.Segment, start, end float64, heard []transcribe.TimestampedWord, window float64) bool {
	if len(heard) == 0 {
		return false
	}
	audible := false
	for _, word := range words {
		if word.RecordingPath != selected.RecordingPath || word.RecordingSHA256 != selected.RecordingSHA256 || spokenWord(word.Text) == "" ||
			(word.Start >= selected.StartTime && word.End <= selected.EndTime) {
			continue
		}
		inWindow := word.End > start-window && word.Start < end+window
		cutOut := word.Start >= start && word.End <= end
		if inWindow && !cutOut {
			audible = true
			break
		}
	}
	return !audible || len(heardAtTheCut(heard, start, window)) > 0
}

// leftoverWords returns the words of the selected segment that heard, a
// transcription of the recording cut from start to end, holds within window
// seconds of the cut point beyond the neighbours' own words there
// (cutWords).
func leftoverWords(words []storage.RefinedWord, selected *transcribe.Segment, start, end float64, heard []transcribe.TimestampedWord, window float64) []string {
	segment, near := cutWords(words, selected, start, end, window)
	var left []string
	for text, count := range heardAtTheCut(heard, start, window) {
		if segment[text] > 0 && count > near[text] {
			left = append(left, text)
		}
	}
	slices.Sort(left)
	return left
}

// spokenWord is a word's letters and digits, lower case.
func spokenWord(text string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(text) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}
