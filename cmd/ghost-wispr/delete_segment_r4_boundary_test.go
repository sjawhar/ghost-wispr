package main

import (
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

func TestR4StraddlingWordKeepsTheBeforeCheck(t *testing.T) {
	words := []audio.Word{{Text: "yes", Start: 0.0, End: 0.5}, {Text: "private", Start: 0.6, End: 0.8}, {Text: "after", Start: 2.0, End: 2.2}}
	start := 0.6 - 0.25
	if !recordingHasWordsOnSide(words, start, 1.05, "yes", true) {
		t.Fatal("PROOF: a neighbour word the cut eats into does not count, so the before-side over-cut check is skipped")
	}
}
