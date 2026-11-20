package transcribe

import "testing"

// TestASpeakerLineNamesItsSpeaker fails if a segment, as a transcript a
// model reads writes it, does not begin with its speaker's label, the id the
// session stores a speaker's name under; or if a segment Deepgram gave no
// speaker (-1) is labelled as a speaker the model could name.
func TestASpeakerLineNamesItsSpeaker(t *testing.T) {
	for _, tc := range []struct {
		speaker int
		want    string
	}{
		{0, "Speaker 0: We ship Friday."},
		{3, "Speaker 3: We ship Friday."},
		{-1, "Unknown speaker: We ship Friday."},
	} {
		seg := Segment{Speaker: tc.speaker, Text: "  We ship Friday. "}
		if got := seg.SpeakerLine(); got != tc.want {
			t.Errorf("speaker %d: SpeakerLine() = %q, want %q", tc.speaker, got, tc.want)
		}
	}
}

// TestWithoutSpeakerLabelsLeavesTheWords fails if WithoutSpeakerLabels
// leaves a label SpeakerLine wrote at the start of a line, or takes away a
// word that is not one: "Speaker" inside a line, or a line with no label.
func TestWithoutSpeakerLabelsLeavesTheWords(t *testing.T) {
	labeled := "Speaker 0: We ship Friday.\nSpeaker 12: The speaker 1 said no.\nUnknown speaker: Fine.\nNo label here.\n"
	want := "We ship Friday.\nThe speaker 1 said no.\nFine.\nNo label here.\n"
	if got := WithoutSpeakerLabels(labeled); got != want {
		t.Errorf("WithoutSpeakerLabels = %q, want %q", got, want)
	}
}
