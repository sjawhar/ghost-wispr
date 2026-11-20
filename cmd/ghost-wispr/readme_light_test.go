package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readmeText returns README.md with its words joined by single spaces, so a
// phrase the Markdown wraps still matches.
func readmeText(t *testing.T) string {
	t.Helper()
	readme, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatalf("read the README: %v", err)
	}
	return strings.Join(strings.Fields(string(readme)), " ")
}

// TestTheReadmeSaysAResumeWithNoMicrophoneLightsNothing fails if the README's
// recording-light section or its POST /api/resume row says only that a
// resume with the light missing is refused: while no capture device is
// present, a resume is accepted and lights nothing (captureGate.RequestResume
// takes no light check then), and the light is checked once a microphone
// can open (ReviewPR77 on #77, round 3).
func TestTheReadmeSaysAResumeWithNoMicrophoneLightsNothing(t *testing.T) {
	readme := readmeText(t)
	for _, words := range []string{
		"While no capture device is present, a resume is accepted and lights nothing",
		"while no capture device is present, it is accepted without a light check",
	} {
		if !strings.Contains(readme, words) {
			t.Errorf("the README does not say %q", words)
		}
	}
}

// TestTheDocsSayADeadMicrophoneLightsRedAtEachRetry fails if the README or
// the runbook does not say that, with a microphone that delivers no audio,
// the light shows red for about 2 s at each retry while nothing is recorded:
// the mic loop opens the microphone again under red after each no-audio wait
// (ReviewPR77 on #77, round 2).
func TestTheDocsSayADeadMicrophoneLightsRedAtEachRetry(t *testing.T) {
	const words = "the light shows red for about 2 s at each retry while nothing is recorded"
	if !strings.Contains(readmeText(t), words) {
		t.Errorf("the README does not say %q", words)
	}
	if section := runbookSection(t, "## Pause and resume at the unit"); !strings.Contains(section, strings.ReplaceAll(words, "2 s", "2 seconds")) {
		t.Errorf("the runbook's \"Pause and resume at the unit\" does not say %q", strings.ReplaceAll(words, "2 s", "2 seconds"))
	}
}

// TestTheReadmeSaysLED1IsTheRecordingLight fails if the README's
// recording-light section does not say that LED 1 alone is the recording
// light the consent check reads back, exactly red, and that LED 2 flashes
// green while an answer waits, only while the unit records, and never counts
// as a light fault (dispatch://INFRA-4623/ask/aef02ac7-90bf-48b9-88df-f42ba932c423).
func TestTheReadmeSaysLED1IsTheRecordingLight(t *testing.T) {
	readme := readmeText(t)
	for _, words := range []string{
		"the recorder captures only while the blink(1)'s LED 1 reads back red",
		"LED 1, the top one (`blink1-tool --led 1`), is the recording light: red means the unit records",
		"Every check reads LED 1 alone back, and only exact red (255,0,0) lets audio through",
		"LED 2, the bottom one, flashes green, half a second on and half a second off, while an agent's answer waits for \"Wispr, go ahead\"",
		"it never flashes while the unit does not record",
		"never counts as a light fault",
		"It flashes only while room audio reaches the recording",
		"a check takes a pattern as black only when each of its lines is black on every LED",
	} {
		if !strings.Contains(readme, words) {
			t.Errorf("the README does not say %q", words)
		}
	}
}
