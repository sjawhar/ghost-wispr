package audio

import (
	"errors"
	"testing"
)

// TestLocateWordsReturnsTheAlignedWordsWithoutAMargin fails if LocateWords
// adds Locate's safety margin: "Delete text" records the segment's aligned
// word span, and refinement drops every word whose middle is in it, so a
// margin would drop a neighbour's word too.
func TestLocateWordsReturnsTheAlignedWordsWithoutAMargin(t *testing.T) {
	words := []Word{
		{Text: "so", Start: 0, End: 0.4},
		{Text: "orange", Start: 0.45, End: 0.9},
		{Text: "tractor", Start: 0.95, End: 1.4},
		{Text: "ninety", Start: 1.45, End: 1.9},
		{Text: "and", Start: 1.95, End: 2.1},
		{Text: "then", Start: 2.15, End: 2.4},
	}
	record, start, end, score, err := LocateWords([]Recording{{Words: words, Duration: 3}}, "Orange tractor ninety.", "so", "and then")
	if err != nil {
		t.Fatalf("LocateWords: %v", err)
	}
	if record != 0 || score != 1 {
		t.Fatalf("record %d score %v, want 0 and 1", record, score)
	}
	requireClose(t, start, 0.45)
	requireClose(t, end, 1.9)

	// The same match, padded, is what Locate returns: the two share one search.
	_, paddedStart, paddedEnd, _, err := Locate([]Recording{{Words: words, Duration: 3}}, "Orange tractor ninety.", "so", "and then")
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}
	requireClose(t, paddedStart, 0.45-locatePadding)
	requireClose(t, paddedEnd, 1.9+locatePadding)
}

// TestLocateWordsRefusesWhatLocateRefuses fails if LocateWords accepts a
// match Locate refuses: a deletion whose span cannot be located is refused.
func TestLocateWordsRefusesWhatLocateRefuses(t *testing.T) {
	fixture := loadLocateFixture(t, "recording.json")
	for name, segment := range map[string]string{
		"weak match": "The server migration completed successfully",
		"empty":      "",
	} {
		if _, _, _, _, err := LocateWords([]Recording{{Words: fixture.Words, Duration: fixture.Duration}}, segment, "", ""); !errors.Is(err, ErrSegmentNotFound) {
			t.Errorf("%s: LocateWords error = %v, want ErrSegmentNotFound", name, err)
		}
	}
	ambiguous := wordsAtHalfSeconds("review the plan then review the plan")
	if _, _, _, _, err := LocateWords([]Recording{{Words: ambiguous, Duration: 4}}, "review the plan", "", ""); !errors.Is(err, ErrSegmentNotFound) {
		t.Errorf("ambiguous repeat: LocateWords error = %v, want ErrSegmentNotFound", err)
	}
}
