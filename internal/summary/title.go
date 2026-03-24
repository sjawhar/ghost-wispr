package summary

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// GenerateTitle applies a 4-level fallback chain to guarantee a non-empty title.
//
//  1. LLM-generated title (if non-empty)
//  2. First substantive sentence from transcript (>5 words, not filler)
//  3. Participants + date from segments ("Speaker 0, Speaker 1 - Mar 23")
//  4. Timestamp-based ("Session 2026-03-23 14:30")
//  5. Ultimate fallback: "Meeting Summary"
func GenerateTitle(llmTitle, transcript string, segments []transcribe.Segment, startedAt time.Time) string {
	// Level 1: LLM-generated title
	if t := strings.TrimSpace(llmTitle); t != "" {
		return normalizeTitle(t)
	}

	// Level 2: First substantive sentence from transcript
	if t := substantiveTitle(transcript); t != "" {
		return t
	}

	// Level 3: Participants + date
	if len(segments) > 0 {
		speakers := uniqueSpeakers(segments)
		if len(speakers) > 0 && !startedAt.IsZero() {
			date := startedAt.Format("Jan 2")
			var label string
			if len(speakers) > 3 {
				label = fmt.Sprintf("%d Speakers", len(speakers))
			} else {
				parts := make([]string, len(speakers))
				for i, s := range speakers {
					parts[i] = fmt.Sprintf("Speaker %d", s)
				}
				label = strings.Join(parts, ", ")
			}
			return normalizeTitle(fmt.Sprintf("%s - %s", label, date))
		}
	}

	// Level 4: Timestamp
	if !startedAt.IsZero() {
		return fmt.Sprintf("Session %s", startedAt.Format("2006-01-02 15:04"))
	}

	// Ultimate fallback
	return "Meeting Summary"
}

// uniqueSpeakers extracts unique speaker numbers from segments, sorted ascending.
func uniqueSpeakers(segments []transcribe.Segment) []int {
	seen := make(map[int]struct{})
	var result []int
	for _, seg := range segments {
		if _, ok := seen[seg.Speaker]; !ok {
			seen[seg.Speaker] = struct{}{}
			result = append(result, seg.Speaker)
		}
	}
	sort.Ints(result)
	return result
}

// guaranteedTitle extracts the first substantive sentence from a transcript.
// substantiveTitle returns a title from the first substantive sentence (>5 words, not filler).
// Returns empty string if no substantive sentence found.
func substantiveTitle(transcript string) string {
	sentences := splitSentences(transcript)
	for _, sentence := range sentences {
		clean := strings.TrimSpace(sentence)
		if clean == "" {
			continue
		}
		words := strings.Fields(clean)
		if len(words) <= 5 {
			continue
		}
		if isFillerSentence(words) {
			continue
		}
		return normalizeTitle(clean)
	}
	return ""
}

// Falls back to any non-empty sentence, then "Meeting Summary".
func guaranteedTitle(transcript string) string {
	sentences := splitSentences(transcript)
	for _, sentence := range sentences {
		clean := strings.TrimSpace(sentence)
		if clean == "" {
			continue
		}
		words := strings.Fields(clean)
		if len(words) <= 5 {
			continue
		}
		if isFillerSentence(words) {
			continue
		}
		return normalizeTitle(clean)
	}
	for _, sentence := range sentences {
		clean := strings.TrimSpace(sentence)
		if clean != "" {
			return normalizeTitle(clean)
		}
	}
	return "Meeting Summary"
}

// splitSentences splits text into sentences on period, exclamation, question mark, and newline.
func splitSentences(text string) []string {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	replacer := strings.NewReplacer("\n", ". ", "!", ". ", "?", ". ")
	normalized := replacer.Replace(text)
	parts := strings.Split(normalized, ".")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

// isFillerSentence returns true if all words in the sentence are filler words.
func isFillerSentence(words []string) bool {
	filler := map[string]struct{}{
		"um": {}, "uh": {}, "yeah": {}, "so": {}, "like": {}, "okay": {}, "ok": {}, "well": {}, "hmm": {}, "huh": {},
	}
	nonFiller := 0
	for _, word := range words {
		clean := strings.TrimFunc(strings.ToLower(word), func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsNumber(r)
		})
		if clean == "" {
			continue
		}
		if _, ok := filler[clean]; ok {
			continue
		}
		nonFiller++
	}
	return nonFiller == 0
}

// normalizeTitle truncates a title to at most 12 words and 100 characters.
func normalizeTitle(title string) string {
	trimmed := strings.TrimSpace(title)
	if trimmed == "" {
		return "Meeting Summary"
	}
	words := strings.Fields(trimmed)
	if len(words) > 12 {
		trimmed = strings.Join(words[:12], " ")
	}
	if utf8.RuneCountInString(trimmed) > 100 {
		runes := []rune(trimmed)
		trimmed = string(runes[:100])
	}
	return strings.TrimSpace(trimmed)
}
