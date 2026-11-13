package audio

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode"

	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

const (
	locatePadding         = 0.25
	minimumLocateScore    = 0.8
	contextDistanceWeight = 0.25
	extraWordCost         = 0.2
	maxContextTokens      = 8
	distanceTolerance     = 1e-9
)

// ErrSegmentNotFound means the recording does not contain a sufficiently strong match.
var ErrSegmentNotFound = errors.New("segment not found in recording")

// Word is one transcribed word with its position in a recording, in seconds.
type Word struct {
	Text       string
	Start, End float64
}

// Recording is one candidate recording to search for a segment.
type Recording struct {
	Words    []Word
	Duration float64
}

// Transcriber returns a recording's words with timestamps.
type Transcriber interface {
	Words(ctx context.Context, path string) ([]Word, error)
}

// WordsTranscriber adapts a batch transcription client to Transcriber.
type WordsTranscriber struct {
	Batch transcribe.BatchWordsTranscriber
}

var _ Transcriber = WordsTranscriber{}

// Words returns timestamped batch transcription words in the audio package's format.
func (t WordsTranscriber) Words(ctx context.Context, path string) ([]Word, error) {
	batchWords, err := t.Batch.Words(ctx, path)
	if err != nil {
		return nil, err
	}

	words := make([]Word, len(batchWords))
	for i, word := range batchWords {
		words[i] = Word{Text: word.Text, Start: word.Start, End: word.End}
	}
	return words, nil
}

type locatedWord struct {
	text string
	word Word
}

type locateCandidate struct {
	record     int
	start, end int
	score      float64
	distance   float64
}

// LocateWords finds the recording and the words of segmentText as Locate
// does, with the same refusals, and returns the start of the first matched
// word and the end of the last, with no safety margin: the segment's aligned
// word span. "Delete text" records it, and refinement drops every word whose
// middle is in it, so a margin would drop a neighbour's word as well.
func LocateWords(recordings []Recording, segmentText, before, after string) (record int, start, end, score float64, err error) {
	best, words, err := locate(recordings, segmentText, before, after)
	if err != nil {
		return -1, 0, 0, best.score, err
	}
	start, end = words[best.start].word.Start, words[best.end-1].word.End
	if !(end > start) {
		return -1, 0, 0, best.score, fmt.Errorf("%w: matched words at %v-%v seconds have no duration", ErrSegmentNotFound, start, end)
	}
	return best.record, start, end, best.score, nil
}

// Locate finds the recording and span of segmentText. Recordings must be in session time
// order so context can cross their boundaries. It chooses the window with the closest
// normalised tokens across every recording, using before and after as lower-weight context,
// then adds a safety margin and clamps to that recording's duration. Doubled boundary words
// and equal-cost ties resolved toward a longer cut usually over-cut one neighbour word, but
// can over-cut more when context is empty or the segment has one or two words. Task 3.2 checks
// every cut afterwards: it refuses if segment words survive or a given neighbour loses more than one word.
func Locate(recordings []Recording, segmentText, before, after string) (record int, start, end, score float64, err error) {
	best, bestWords, err := locate(recordings, segmentText, before, after)
	if err != nil {
		return -1, 0, 0, best.score, err
	}
	start = clamp(bestWords[best.start].word.Start-locatePadding, 0, recordings[best.record].Duration)
	end = clamp(bestWords[best.end-1].word.End+locatePadding, 0, recordings[best.record].Duration)
	if !(end > start) {
		return -1, 0, 0, best.score, fmt.Errorf("%w: matched words at %v-%v seconds lie outside the recording's %v-second duration",
			ErrSegmentNotFound, bestWords[best.start].word.Start, bestWords[best.end-1].word.End, recordings[best.record].Duration)
	}
	return best.record, start, end, best.score, nil
}

// locate is the search Locate and LocateWords share: the best window and the
// normalised words of its recording, or ErrSegmentNotFound for no match, a
// weak or ambiguous one, or one whose context matches better inside it.
func locate(recordings []Recording, segmentText, before, after string) (best locateCandidate, bestWords []locatedWord, err error) {
	segmentTokens := normaliseText(segmentText)
	if len(segmentTokens) == 0 {
		return best, nil, ErrSegmentNotFound
	}

	beforeTokens := normaliseText(before)
	afterTokens := normaliseText(after)
	windowSlack := max(3, len(segmentTokens)/2)
	maxTokenCount := max(len(segmentTokens), len(beforeTokens), len(afterTokens))
	maxWindowSize := len(segmentTokens) + windowSlack
	maxTokenCount = max(maxTokenCount, maxWindowSize)
	previous := make([]float64, maxTokenCount+1)
	current := make([]float64, maxTokenCount+1)
	lcsPrevious := make([]int, maxTokenCount+1)
	lcsCurrent := make([]int, maxTokenCount+1)

	recordingWordsByRecord := make([][]locatedWord, len(recordings))
	recordingTokensByRecord := make([][]string, len(recordings))
	for recordIndex, recording := range recordings {
		recordingWordsByRecord[recordIndex] = normaliseWords(recording.Words)
		recordingTokensByRecord[recordIndex] = wordTexts(recordingWordsByRecord[recordIndex])
	}
	beforeContext := make([]string, maxContextTokens)
	afterContext := make([]string, maxContextTokens)

	minWindowSize := max(1, len(segmentTokens)-windowSlack)
	found := false
	ambiguous := false
	for recordIndex := range recordings {
		recordingWords := recordingWordsByRecord[recordIndex]
		if len(recordingWords) == 0 {
			continue
		}
		recordingTokens := recordingTokensByRecord[recordIndex]

		var afterDistances []float64
		if len(afterTokens) > 0 {
			afterDistances = make([]float64, len(recordingTokens)+1)
			for windowEnd := range afterDistances {
				afterDistances[windowEnd] = boundaryDistance(afterTokens, recordingTokensByRecord, recordIndex, windowEnd, false, afterContext, previous, current)
			}
		}

		for windowStart := range recordingWords {
			beforeDistance := boundaryDistance(beforeTokens, recordingTokensByRecord, recordIndex, windowStart, true, beforeContext, previous, current)
			windowEndLimit := min(len(recordingWords), windowStart+maxWindowSize)
			windowTokens := recordingTokens[windowStart:windowEndLimit]
			distanceRow := weightedEditDistanceRow(segmentTokens, windowTokens, previous, current)
			scoreRow := longestCommonSubsequenceRow(segmentTokens, windowTokens, lcsPrevious, lcsCurrent)
			for windowEnd := windowStart + minWindowSize; windowEnd <= windowEndLimit; windowEnd++ {
				offset := windowEnd - windowStart
				afterDistance := 0.0
				if afterDistances != nil {
					afterDistance = afterDistances[windowEnd]
				}
				distance := distanceRow[offset] + contextDistanceWeight*(beforeDistance+afterDistance)
				if found && distance > best.distance+distanceTolerance {
					continue
				}

				candidate := locateCandidate{
					record:   recordIndex,
					start:    windowStart,
					end:      windowEnd,
					score:    float64(scoreRow[offset]) / float64(max(len(segmentTokens), offset)),
					distance: distance,
				}
				switch {
				case !found || candidate.distance < best.distance-distanceTolerance:
					best = candidate
					bestWords = recordingWords
					found = true
					ambiguous = false
				case candidate.record != best.record || (candidate.start != best.start && candidate.end != best.end):
					ambiguous = true
				// On equal evidence, prefer the longer cut because a single leftover
				// word the requester named is the worse failure. Task 3.2's survivor
				// check validates that later.
				case candidate.score > best.score || (candidate.score == best.score && candidate.end-candidate.start > best.end-best.start):
					best = candidate
				}
			}
		}
	}

	if !found || ambiguous || best.score < minimumLocateScore {
		return best, nil, ErrSegmentNotFound
	}

	if contextMatchesBetterInside(beforeTokens, recordingTokensByRecord, best, true, beforeContext, previous, current) ||
		contextMatchesBetterInside(afterTokens, recordingTokensByRecord, best, false, afterContext, previous, current) {
		return best, nil, ErrSegmentNotFound
	}
	return best, bestWords, nil
}

func normaliseWords(words []Word) []locatedWord {
	normalised := make([]locatedWord, 0, len(words))
	for _, word := range words {
		for _, token := range normaliseText(word.Text) {
			normalised = append(normalised, locatedWord{text: token, word: word})
		}
	}
	return normalised
}

func normaliseText(text string) []string {
	fields := strings.Fields(text)
	tokens := make([]string, 0, len(fields))
	for _, field := range fields {
		var token strings.Builder
		for _, r := range field {
			if unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.IsMark(r) {
				token.WriteRune(unicode.ToLower(r))
			}
		}
		if token.Len() > 0 {
			tokens = append(tokens, token.String())
		}
	}
	return tokens
}

func wordTexts(words []locatedWord) []string {
	texts := make([]string, len(words))
	for i, word := range words {
		texts[i] = word.text
	}
	return texts
}

func boundaryDistance(context []string, recordings [][]string, record, boundary int, before bool, scratch []string, previous, current []float64) float64 {
	if len(context) == 0 {
		return 0
	}

	tokenCount := min(len(context), maxContextTokens)
	if before {
		write := tokenCount
		for recordIndex := record; recordIndex >= 0 && write > 0; recordIndex-- {
			end := len(recordings[recordIndex])
			if recordIndex == record {
				end = boundary
			}
			count := min(end, write)
			write -= count
			copy(scratch[write:write+count], recordings[recordIndex][end-count:end])
		}
		return weightedEditDistance(context[len(context)-tokenCount:], scratch[write:tokenCount], previous, current)
	}

	count := 0
	for recordIndex := record; recordIndex < len(recordings) && count < tokenCount; recordIndex++ {
		start := 0
		if recordIndex == record {
			start = boundary
		}
		available := min(len(recordings[recordIndex])-start, tokenCount-count)
		copy(scratch[count:count+available], recordings[recordIndex][start:start+available])
		count += available
	}
	return weightedEditDistance(context[:tokenCount], scratch[:count], previous, current)
}

func contextMatchesBetterInside(context []string, recordings [][]string, candidate locateCandidate, before bool, scratch []string, previous, current []float64) bool {
	if len(context) == 0 {
		return false
	}

	edge := candidate.end
	if before {
		edge = candidate.start
	}
	edgeDistance := boundaryDistance(context, recordings, candidate.record, edge, before, scratch, previous, current)
	for boundary := candidate.start + 1; boundary < candidate.end; boundary++ {
		if boundaryDistance(context, recordings, candidate.record, boundary, before, scratch, previous, current) < edgeDistance-distanceTolerance {
			return true
		}
	}
	return false
}

func weightedEditDistance(want, got []string, previous, current []float64) float64 {
	return weightedEditDistanceRow(want, got, previous, current)[len(got)]
}

func weightedEditDistanceRow(want, got []string, previous, current []float64) []float64 {
	for j := 0; j <= len(got); j++ {
		previous[j] = float64(j) * extraWordCost
	}
	for i, wanted := range want {
		current[0] = float64(i + 1)
		for j, actual := range got {
			substitution := previous[j]
			if wanted != actual {
				substitution++
			}
			deletion := previous[j+1] + 1
			insertion := current[j] + extraWordCost
			current[j+1] = min(substitution, deletion, insertion)
		}
		previous, current = current, previous
	}
	return previous
}

func longestCommonSubsequenceRow(left, right []string, previous, current []int) []int {
	clear(previous[:len(right)+1])
	for _, leftToken := range left {
		current[0] = 0
		for j, rightToken := range right {
			if leftToken == rightToken {
				current[j+1] = previous[j] + 1
			} else {
				current[j+1] = max(previous[j+1], current[j])
			}
		}
		previous, current = current, previous
	}
	return previous
}

func clamp(value, lower, upper float64) float64 {
	return math.Min(math.Max(value, lower), upper)
}
