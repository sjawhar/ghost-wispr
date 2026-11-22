package summary

import (
	"context"
	"strings"
	"testing"
)

// A summary reads its transcript with each segment's speaker label ("Speaker
// 0: …"), so the model can name a speaker by the id the session stores the
// name under. The labels are not words of the conversation.

// TestAShortLabelledTranscriptIsTitledByItsWords fails if a transcript too
// short to summarize is counted with its labels, so that a few words in
// several segments reach the model, or is titled with a label.
func TestAShortLabelledTranscriptIsTitledByItsWords(t *testing.T) {
	// 12 words in 5 segments: 22 with the labels, past the 20 a summary needs.
	transcript := "Speaker 0: Turn off the lights.\nSpeaker 1: Which ones?\nSpeaker 0: The porch.\nSpeaker 1: Done.\nSpeaker 0: Thanks a lot.\n"
	client := &mockLLMClient{response: `{"title":"Porch lights","summary":"Lights off.","speakers":[]}`}
	title, summaryText, _, err := summarizerFor(client).SummarizeWithPreset(context.Background(), "session-1", transcript, "default")
	if err != nil {
		t.Fatal(err)
	}
	if client.calls != 0 || summaryText != "" {
		t.Errorf("a 12-word transcript asked the model %d times and got summary %q; want no call: its labels are not words", client.calls, summaryText)
	}
	if title != "Turn off the lights" {
		t.Errorf("title = %q, want the first sentence's words, %q, without its speaker label", title, "Turn off the lights")
	}
}

// TestATitleMadeFromALabelledTranscriptHasNoLabel fails if the title made
// from the transcript, when the model's answer has no title, keeps a speaker
// label.
func TestATitleMadeFromALabelledTranscriptHasNoLabel(t *testing.T) {
	transcript := "Speaker 1: " + buildTranscript(25) + ".\nSpeaker 0: Agreed.\n"
	client := &mockLLMClient{response: `{"title":"...","summary":"The team agreed.","speakers":[]}`}
	title, _, _, err := summarizerFor(client).SummarizeWithPreset(context.Background(), "session-1", transcript, "default")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(title, "Speaker") || !strings.HasPrefix(title, "word word") {
		t.Errorf("title = %q, want the transcript's first words without a speaker label", title)
	}
	if user := client.lastMessages[len(client.lastMessages)-1].Content; !strings.Contains(user, "Speaker 1: word word") || !strings.Contains(user, "Speaker 0: Agreed.") {
		t.Errorf("the prompt does not carry the transcript's speaker labels:\n%s", user)
	}
}
