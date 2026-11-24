package summary

import (
	"context"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/llm"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// mainShortTitle is the title main 442af5e gave a transcript too short to
// summarize (summarizer.go:149-150 there): guaranteedTitle of the transcript
// without its speaker labels.
func mainShortTitle(transcript string) string {
	return guaranteedTitle(transcribe.WithoutSpeakerLabels(transcript))
}

// TestAShortTranscriptKeepsMainsTitle fails if a transcript too short to
// summarize is titled otherwise than main titled it, with one preset or with
// two: checking its length before the preset call must change no stored
// title. Each want is the title main 442af5e stored for that transcript,
// read from its summarizer; mainShortTitle is main's title path. A segment
// whose words begin with a label-shaped prefix ("Speaker 0: Speaker 1: …")
// is the case a single strip of the labels titles differently.
func TestAShortTranscriptKeepsMainsTitle(t *testing.T) {
	for _, tc := range []struct{ transcript, want string }{
		{"Speaker 0: Speaker 1: Turn off the lights.", "Turn off the lights"},
		{"Speaker 0: Turn off the lights.", "Turn off the lights"},
		{"Speaker 0: Speaker 1: Speaker 2: Turn off the lights.", "Speaker 2: Turn off the lights"},
		{"Unknown speaker: Speaker 3: Which ones?\nSpeaker 1: The porch.", "Which ones"},
		{"Turn off the lights.", "Turn off the lights"},
		{"Speaker 0: ", "Meeting Summary"},
		{"", "Meeting Summary"},
		{"Speaker 0: um\nSpeaker 1: uh huh", "um"},
		{"Speaker 0: Please move the porch light to the left side now.\nSpeaker 1: Speaker 2: ok", "Please move the porch light to the left side now"},
		{"Speaker 0: Speaker 0: Speaker 0: Speaker 0: done", "Speaker 0: Speaker 0: done"},
		{"The note said\nSpeaker 2: hello there.", "The note said"},
		{"Speaker 1: Speaker 1:Turn it off.", "Speaker 1:Turn it off"},
	} {
		if got := mainShortTitle(tc.transcript); got != tc.want {
			t.Fatalf("main's title path gives %q for %q, want %q: the reference no longer matches main", got, tc.transcript, tc.want)
		}
		for _, presets := range []map[string]config.Preset{
			{"default": {SystemPrompt: "system", UserTemplate: "{{transcript}}"}},
			{"default": {SystemPrompt: "system", UserTemplate: "{{transcript}}"}, "engineering": {SystemPrompt: "system", UserTemplate: "{{transcript}}"}},
		} {
			client := &mockLLMClient{response: "default"}
			s := New(config.Summarization{Model: "openai/gpt-4o-mini", Presets: presets}, func(_, _ string) (llm.Client, error) {
				return client, nil
			})
			title, summaryText, _, _, err := s.Summarize(context.Background(), "session-1", tc.transcript)
			if err != nil {
				t.Fatalf("Summarize(%q) with %d presets: %v", tc.transcript, len(presets), err)
			}
			if title != tc.want || summaryText != "" || client.calls != 0 {
				t.Errorf("Summarize(%q) with %d presets: title %q, summary %q, %d model calls; want main's title %q, no summary, no call",
					tc.transcript, len(presets), title, summaryText, client.calls, tc.want)
			}
		}
	}
}
