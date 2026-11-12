package audio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

type locateFixture struct {
	Duration float64 `json:"duration"`
	Words    []Word  `json:"words"`
}

func loadLocateFixture(t *testing.T, name string) locateFixture {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("testdata", "locate", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	var fixture locateFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return fixture
}

func requireClose(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 0.000001 {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func locateOne(words []Word, segmentText, before, after string, duration float64) (start, end, score float64, err error) {
	_, start, end, score, err = Locate([]Recording{{Words: words, Duration: duration}}, segmentText, before, after)
	return start, end, score, err
}

func wordsAtHalfSeconds(text string) []Word {
	tokens := strings.Fields(text)
	words := make([]Word, len(tokens))
	for i, token := range tokens {
		words[i] = Word{Text: token, Start: float64(i) * 0.5, End: float64(i+1) * 0.5}
	}
	return words
}

type timestampedWordsFixture struct{}

func (timestampedWordsFixture) Transcribe(context.Context, string) (transcribe.BatchResult, error) {
	return transcribe.BatchResult{Transcript: "refined text"}, nil
}

func (timestampedWordsFixture) Words(context.Context, string) ([]transcribe.TimestampedWord, error) {
	return []transcribe.TimestampedWord{{Text: "recorded", Start: 0.3, End: 0.8}}, nil
}

func TestWordsTranscriberConvertsBatchWordTimestamps(t *testing.T) {
	words, err := (WordsTranscriber{Batch: timestampedWordsFixture{}}).Words(context.Background(), "recording.mp3")
	if err != nil {
		t.Fatalf("Words returned error: %v", err)
	}
	if len(words) != 1 || words[0] != (Word{Text: "recorded", Start: 0.3, End: 0.8}) {
		t.Fatalf("Words = %#v, want one converted timestamped word", words)
	}
}

func TestLocateExactMatch(t *testing.T) {
	fixture := loadLocateFixture(t, "recording.json")

	start, end, score, err := locateOne(fixture.Words, "We will review the release.", "", "", fixture.Duration)
	if err != nil {
		t.Fatalf("Locate returned error: %v", err)
	}

	requireClose(t, start, 0.55)
	requireClose(t, end, 2.55)
	requireClose(t, score, 1)
}

func TestLocateToleratesLiveTranscriptDifferences(t *testing.T) {
	words := []Word{
		{Text: "Hello,", Start: 0.0, End: 0.3},
		{Text: "I", Start: 0.3, End: 0.45},
		{Text: "THINK", Start: 0.45, End: 0.7},
		{Text: "we", Start: 0.7, End: 0.85},
		{Text: "should", Start: 0.85, End: 1.15},
		{Text: "um", Start: 1.15, End: 1.35},
		{Text: "ship", Start: 1.35, End: 1.6},
		{Text: "the", Start: 1.6, End: 1.75},
		{Text: "new", Start: 1.75, End: 2.0},
		{Text: "plan", Start: 2.0, End: 2.25},
		{Text: "today!", Start: 2.25, End: 2.55},
	}

	start, end, score, err := locateOne(words, "i think, we should ship the noon plan today", "", "", 3)
	if err != nil {
		t.Fatalf("Locate returned error: %v", err)
	}

	requireClose(t, start, 0.05)
	requireClose(t, end, 2.8)
	if score < 0.8 {
		t.Fatalf("score = %v, want at least 0.8", score)
	}
}

func TestLocateRefusesWeakMatches(t *testing.T) {
	fixture := loadLocateFixture(t, "recording.json")

	_, _, _, err := locateOne(fixture.Words, "The server migration completed successfully", "", "", fixture.Duration)
	if !errors.Is(err, ErrSegmentNotFound) {
		t.Fatalf("Locate error = %v, want ErrSegmentNotFound", err)
	}
}

func TestLocateEnforcesEightyPercentMatch(t *testing.T) {
	segmentText := "alpha beta gamma delta epsilon"

	weakWords := []Word{
		{Text: "alpha", Start: 0, End: 0.2},
		{Text: "beta", Start: 0.2, End: 0.4},
		{Text: "gamma", Start: 0.4, End: 0.6},
		{Text: "zeta", Start: 0.6, End: 0.8},
		{Text: "eta", Start: 0.8, End: 1},
	}
	_, _, _, err := locateOne(weakWords, segmentText, "", "", 1)
	if !errors.Is(err, ErrSegmentNotFound) {
		t.Fatalf("Locate error = %v, want ErrSegmentNotFound below 80%%", err)
	}

	thresholdWords := []Word{
		{Text: "alpha", Start: 0, End: 0.2},
		{Text: "beta", Start: 0.2, End: 0.4},
		{Text: "gamma", Start: 0.4, End: 0.6},
		{Text: "delta", Start: 0.6, End: 0.8},
		{Text: "eta", Start: 0.8, End: 1},
	}

	// The final word is misheard. The equal-cost longer window is intentional:
	// a consent deletion must not leave that named word in the recording.
	_, end, score, err := locateOne(thresholdWords, segmentText, "", "", 2)
	if err != nil {
		t.Fatalf("Locate returned error at 80%% match: %v", err)
	}
	requireClose(t, end, 1.25)
	requireClose(t, score, 0.8)
}

func TestLocateRefusesEmptyInput(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		words       []Word
		segmentText string
	}{
		{name: "no recording words", segmentText: "recorded text"},
		{name: "no segment words", words: []Word{{Text: "recorded", Start: 0, End: 1}}, segmentText: "?!"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, _, _, err := locateOne(testCase.words, testCase.segmentText, "", "", 1)
			if !errors.Is(err, ErrSegmentNotFound) {
				t.Fatalf("Locate error = %v, want ErrSegmentNotFound", err)
			}
		})
	}
}

func TestLocateClampsAtTheEdges(t *testing.T) {
	t.Run("start", func(t *testing.T) {
		words := []Word{
			{Text: "Beginning", Start: 0.1, End: 0.3},
			{Text: "here", Start: 0.3, End: 0.55},
		}

		start, _, _, err := locateOne(words, "beginning here", "", "", 1)
		if err != nil {
			t.Fatalf("Locate returned error: %v", err)
		}
		requireClose(t, start, 0)
	})

	t.Run("end", func(t *testing.T) {
		words := []Word{
			{Text: "Almost", Start: 9.35, End: 9.65},
			{Text: "done", Start: 9.65, End: 9.95},
		}

		_, end, _, err := locateOne(words, "almost done", "", "", 10)
		if err != nil {
			t.Fatalf("Locate returned error: %v", err)
		}
		requireClose(t, end, 10)
	})
}

func TestLocateRefusesUnusableDuration(t *testing.T) {
	words := []Word{
		{Text: "Almost", Start: 9.35, End: 9.65},
		{Text: "done", Start: 9.65, End: 9.95},
	}

	for _, testCase := range []struct {
		name     string
		duration float64
	}{
		{name: "before match", duration: 9},
		{name: "negative", duration: -1},
		{name: "not a number", duration: math.NaN()},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, _, _, err := locateOne(words, "almost done", "", "", testCase.duration)
			if !errors.Is(err, ErrSegmentNotFound) {
				t.Fatalf("Locate error = %v, want ErrSegmentNotFound", err)
			}
		})
	}
}

func TestLocatePicksTheRightRepeat(t *testing.T) {
	fixture := loadLocateFixture(t, "repeats.json")

	start, end, score, err := locateOne(fixture.Words, "review the plan", "after lunch we will", "carefully", fixture.Duration)
	if err != nil {
		t.Fatalf("Locate returned error: %v", err)
	}

	requireClose(t, start, 3.75)
	requireClose(t, end, 5.15)
	requireClose(t, score, 1)
}

func TestLocateUsesContextAcrossRecordingBoundaries(t *testing.T) {
	t.Run("before", func(t *testing.T) {
		recordings := []Recording{
			{Duration: 10, Words: wordsAtHalfSeconds("and we start review the plan soon we start now")},
			{Duration: 4, Words: wordsAtHalfSeconds("review the plan carefully")},
		}

		record, _, _, _, err := Locate(recordings, "review the plan", "we start now", "carefully")
		if err != nil {
			t.Fatalf("Locate returned error: %v", err)
		}
		if record != 1 {
			t.Fatalf("record = %d, want 1", record)
		}
	})

	t.Run("after", func(t *testing.T) {
		recordings := []Recording{
			{Duration: 4, Words: wordsAtHalfSeconds("review the plan")},
			{Duration: 10, Words: wordsAtHalfSeconds("carefully and we start review the plan soon")},
		}

		record, _, _, _, err := Locate(recordings, "review the plan", "", "carefully")
		if err != nil {
			t.Fatalf("Locate returned error: %v", err)
		}
		if record != 0 {
			t.Fatalf("record = %d, want 0", record)
		}
	})
}

func TestLocateRefusesAmbiguousRepeat(t *testing.T) {
	words := []Word{
		{Text: "review", Start: 0, End: 0.3},
		{Text: "the", Start: 0.3, End: 0.5},
		{Text: "plan", Start: 0.5, End: 0.9},
		{Text: "then", Start: 0.9, End: 1.1},
		{Text: "review", Start: 1.1, End: 1.4},
		{Text: "the", Start: 1.4, End: 1.6},
		{Text: "plan", Start: 1.6, End: 2},
	}

	_, _, _, err := locateOne(words, "review the plan", "", "", 3)
	if !errors.Is(err, ErrSegmentNotFound) {
		t.Fatalf("Locate error = %v, want ErrSegmentNotFound for an ambiguous match", err)
	}
}

func TestLocateRefusesWindowThatCutsTheNextSegment(t *testing.T) {
	words := []Word{
		{Text: "so", Start: 0, End: 0.5},
		{Text: "today", Start: 0.5, End: 1.0},
		{Text: "we", Start: 1.0, End: 1.5},
		{Text: "should", Start: 1.5, End: 2.0},
		{Text: "ask", Start: 2.0, End: 2.5},
		{Text: "Pam.", Start: 2.5, End: 3.0},
		{Text: "Then", Start: 3.0, End: 3.5},
		{Text: "Sam", Start: 3.5, End: 4.0},
		{Text: "said", Start: 4.0, End: 4.5},
		{Text: "the", Start: 4.5, End: 5.0},
		{Text: "budget", Start: 5.0, End: 5.5},
		{Text: "is", Start: 5.5, End: 6.0},
		{Text: "fine", Start: 6.0, End: 6.5},
	}

	_, _, _, err := locateOne(words, "we should ask Sam.", "so today", "Then Sam said the budget is fine", 10)
	if !errors.Is(err, ErrSegmentNotFound) {
		t.Fatalf("Locate error = %v, want ErrSegmentNotFound", err)
	}
}

func TestLocateWeightsSegmentAboveContext(t *testing.T) {
	words := []Word{
		{Text: "after", Start: 0, End: 0.2},
		{Text: "lunch", Start: 0.2, End: 0.4},
		{Text: "today", Start: 0.4, End: 0.7},
		{Text: "review", Start: 0.7, End: 1},
		{Text: "the", Start: 1, End: 1.2},
		{Text: "wrong", Start: 1.2, End: 1.5},
		{Text: "after", Start: 3, End: 3.2},
		{Text: "breakfast", Start: 3.2, End: 3.5},
		{Text: "tomorrow", Start: 3.5, End: 3.8},
		{Text: "review", Start: 3.8, End: 4.1},
		{Text: "the", Start: 4.1, End: 4.3},
		{Text: "plan", Start: 4.3, End: 4.7},
	}

	start, _, _, err := locateOne(words, "review the plan", "after lunch today", "", 5)
	if err != nil {
		t.Fatalf("Locate returned error: %v", err)
	}
	requireClose(t, start, 3.55)
}

func TestLocateRanksRecordingsByContext(t *testing.T) {
	recordings := []Recording{
		{Duration: 2, Words: []Word{
			{Text: "after", Start: 0, End: 0.2},
			{Text: "breakfast", Start: 0.2, End: 0.5},
			{Text: "review", Start: 0.5, End: 0.8},
			{Text: "plan", Start: 0.8, End: 1.1},
			{Text: "carefully", Start: 1.1, End: 1.5},
		}},
		{Duration: 2, Words: []Word{
			{Text: "after", Start: 0, End: 0.2},
			{Text: "lunch", Start: 0.2, End: 0.5},
			{Text: "review", Start: 0.5, End: 0.8},
			{Text: "plan", Start: 0.8, End: 1.1},
			{Text: "carefully", Start: 1.1, End: 1.5},
		}},
	}

	record, start, _, score, err := Locate(recordings, "review plan", "after lunch", "carefully")
	if err != nil {
		t.Fatalf("Locate returned error: %v", err)
	}
	if record != 1 {
		t.Fatalf("record = %d, want 1", record)
	}
	requireClose(t, start, 0.25)
	requireClose(t, score, 1)
}

func TestLocateRefusesAmbiguousRecordings(t *testing.T) {
	words := []Word{
		{Text: "after", Start: 0, End: 0.2},
		{Text: "lunch", Start: 0.2, End: 0.5},
		{Text: "review", Start: 0.5, End: 0.8},
		{Text: "plan", Start: 0.8, End: 1.1},
		{Text: "carefully", Start: 1.1, End: 1.5},
	}
	recordings := []Recording{{Words: words, Duration: 2}, {Words: words, Duration: 2}}

	_, _, _, _, err := Locate(recordings, "review plan", "after lunch", "carefully")
	if !errors.Is(err, ErrSegmentNotFound) {
		t.Fatalf("Locate error = %v, want ErrSegmentNotFound", err)
	}
}

func TestLocateRefusesLongWindowThatCutsTheNextSegment(t *testing.T) {
	segmentTokens := make([]string, 25)
	for i := range segmentTokens {
		segmentTokens[i] = fmt.Sprintf("word%d", i)
	}
	segmentTokens = append(segmentTokens, "ask", "Sam")
	recordingTokens := append([]string{}, segmentTokens[:len(segmentTokens)-1]...)
	recordingTokens[len(recordingTokens)-1] = "Pam"
	recordingTokens = append(recordingTokens, "Then", "Sam", "said", "fine")

	_, _, _, err := locateOne(wordsAtHalfSeconds(strings.Join(recordingTokens, " ")), strings.Join(segmentTokens, " "), "", "Then Sam said fine", 30)
	if !errors.Is(err, ErrSegmentNotFound) {
		t.Fatalf("Locate error = %v, want ErrSegmentNotFound", err)
	}
}

func TestLocateRefusesBeforeSideInteriorMatch(t *testing.T) {
	segmentTokens := make([]string, 23)
	for i := range segmentTokens {
		segmentTokens[i] = fmt.Sprintf("before-word%d", i)
	}
	segmentTokens = append([]string{"Sam"}, segmentTokens...)

	for _, testCase := range []struct {
		name, before string
		prefix       []string
	}{
		{name: "long", before: "fine said then Sam", prefix: []string{"fine", "said", "then", "Sam", "Pam"}},
		{name: "adjacent", before: "fine said Sam", prefix: []string{"fine", "said", "Sam", "Pam"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			recordingTokens := append([]string{}, testCase.prefix...)
			recordingTokens = append(recordingTokens, segmentTokens[1:]...)
			recordingTokens = append(recordingTokens, "okay", "next")

			_, _, _, err := locateOne(wordsAtHalfSeconds(strings.Join(recordingTokens, " ")), strings.Join(segmentTokens, " "), testCase.before, "okay next", 30)
			if !errors.Is(err, ErrSegmentNotFound) {
				t.Fatalf("Locate error = %v, want ErrSegmentNotFound", err)
			}
		})
	}
}

func TestLocateScoresTheEntireCutWindow(t *testing.T) {
	_, _, _, err := locateOne(wordsAtHalfSeconds("alpha beta um so anyway kappa"), "alpha beta kappa", "", "", 3)
	if !errors.Is(err, ErrSegmentNotFound) {
		t.Fatalf("Locate error = %v, want ErrSegmentNotFound", err)
	}
}

func TestLocateNeverCutsAnUneditedNeighbourWord(t *testing.T) {
	rng := rand.New(rand.NewPCG(42, 17))
	for caseIndex := range 32 {
		prefixLength := 8 + rng.IntN(24)
		segmentTokens := make([]string, prefixLength)
		recordingTokens := make([]string, prefixLength)
		for i := range segmentTokens {
			token := fmt.Sprintf("segment-%d-%d", caseIndex, i)
			segmentTokens[i] = token
			recordingTokens[i] = token
		}
		wantedLast := fmt.Sprintf("wanted-%d", caseIndex)
		actualLast := fmt.Sprintf("actual-%d", caseIndex)
		segmentTokens = append(segmentTokens, wantedLast)
		recordingTokens = append(recordingTokens, actualLast)
		afterTokens := []string{wantedLast, fmt.Sprintf("after-%d", caseIndex), "continues"}
		recordingTokens = append(recordingTokens, afterTokens...)

		// Doubled actual boundary words are a documented limit and are deliberately
		// not generated by this property.
		words := wordsAtHalfSeconds(strings.Join(recordingTokens, " "))
		_, end, _, err := locateOne(words, strings.Join(segmentTokens, " "), "", strings.Join(afterTokens, " "), 60)
		if err == nil {
			segmentEnd := float64(prefixLength+1) * 0.5
			if end > segmentEnd+0.250001 {
				t.Fatalf("case %d cut into the next segment: end=%v, segment end=%v", caseIndex, end, segmentEnd)
			}
		}

		wantedFirst := fmt.Sprintf("wanted-first-%d", caseIndex)
		actualFirst := fmt.Sprintf("actual-first-%d", caseIndex)
		beforeTokens := []string{"context", wantedFirst}
		beforeRecording := append([]string{}, beforeTokens...)
		beforeRecording = append(beforeRecording, actualFirst)
		beforeRecording = append(beforeRecording, segmentTokens...)
		beforeRecording = append(beforeRecording, "tail")

		beforeSegment := append([]string{wantedFirst}, segmentTokens...)
		start, _, _, err := locateOne(wordsAtHalfSeconds(strings.Join(beforeRecording, " ")), strings.Join(beforeSegment, " "), strings.Join(beforeTokens, " "), "tail", 60)
		if err == nil && start < 0.75 {
			t.Fatalf("case %d cut into the previous segment: start=%v", caseIndex, start)
		}
	}
}

func TestLocateRefusesOverlappingRepeatedWords(t *testing.T) {
	for _, testCase := range []struct {
		name, segment string
		words         []Word
	}{
		{name: "no", segment: "no no", words: wordsAtHalfSeconds("no no no")},
		{name: "yeah", segment: "yeah yeah yeah", words: wordsAtHalfSeconds("yeah yeah yeah yeah")},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, _, _, err := locateOne(testCase.words, testCase.segment, "", "", 2)
			if !errors.Is(err, ErrSegmentNotFound) {
				t.Fatalf("Locate error = %v, want ErrSegmentNotFound", err)
			}
		})
	}
}
