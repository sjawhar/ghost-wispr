package gdrive

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

type SyncSession struct {
	ID            string
	Title         string
	StartedAt     time.Time
	EndedAt       *time.Time
	Summary       string
	SummaryPreset string
}

func RenderSummaryMarkdown(s SyncSession) string {
	var b strings.Builder

	b.WriteString("---\n")
	b.WriteString("schema_version: 1\n")
	fmt.Fprintf(&b, "id: %q\n", s.ID)
	fmt.Fprintf(&b, "started_at: %q\n", s.StartedAt.UTC().Format(time.RFC3339))
	if s.EndedAt != nil {
		fmt.Fprintf(&b, "ended_at: %q\n", s.EndedAt.UTC().Format(time.RFC3339))
	}
	if s.SummaryPreset != "" {
		fmt.Fprintf(&b, "summary_preset: %q\n", s.SummaryPreset)
	}
	b.WriteString("---\n\n")

	title := s.Title
	if title == "" {
		title = s.ID
	}
	fmt.Fprintf(&b, "# %s\n\n", title)
	b.WriteString(s.Summary)
	b.WriteString("\n")

	return b.String()
}

func RenderTranscriptMarkdown(s SyncSession, segments []transcribe.Segment) string {
	var b strings.Builder

	speakerSet := make(map[int]struct{})
	for _, seg := range segments {
		speakerSet[seg.Speaker] = struct{}{}
	}
	speakers := make([]int, 0, len(speakerSet))
	for sp := range speakerSet {
		speakers = append(speakers, sp)
	}
	sort.Ints(speakers)

	b.WriteString("---\n")
	b.WriteString("schema_version: 1\n")
	fmt.Fprintf(&b, "id: %q\n", s.ID)
	fmt.Fprintf(&b, "started_at: %q\n", s.StartedAt.UTC().Format(time.RFC3339))
	if s.EndedAt != nil {
		fmt.Fprintf(&b, "ended_at: %q\n", s.EndedAt.UTC().Format(time.RFC3339))
	}
	if len(speakers) > 0 {
		b.WriteString("speakers:\n")
		for _, sp := range speakers {
			fmt.Fprintf(&b, "  - \"Speaker %d\"\n", sp+1)
		}
	}
	b.WriteString("---\n\n")

	b.WriteString("# Transcript\n\n")

	for _, seg := range segments {
		ts := seg.Timestamp.UTC().Format("15:04:05")
		text := strings.TrimSpace(seg.Text)
		if text == "" {
			continue
		}
		fmt.Fprintf(&b, "**[%s] Speaker %d:** %s\n\n", ts, seg.Speaker+1, text)
	}

	return b.String()
}
