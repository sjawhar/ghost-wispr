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
