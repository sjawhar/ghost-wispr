//go:build kws && linux

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const clips = "../../internal/wake/local/testdata"

// copyClip copies a stored clip into dir.
func copyClip(t *testing.T, name, dir string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(clips, name))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestSplicedPhrasesAreNotFalseStarts fails if the run counts a phrase it
// spliced in as a control as a false start, misses it, or counts a false start
// in speech with no phrase.
func TestSplicedPhrasesAreNotFalseStarts(t *testing.T) {
	root := t.TempDir()
	copyClip(t, "stop.wav", filepath.Join(root, "audio", "chapter"))
	copyClip(t, "start.wav", filepath.Join(root, "splice"))
	var out bytes.Buffer
	code := run([]string{"-audio", filepath.Join(root, "audio"), "-splice", filepath.Join(root, "splice"), "-splice-every", "1ms", "-min-hours", "0"}, &out, &out)
	text := out.String()
	if code != 0 || !strings.Contains(text, "false_starts=0 ") || !strings.Contains(text, "spliced_clips=1 spliced_heard=1") || !strings.HasSuffix(strings.TrimSpace(text), "PASS") {
		t.Fatalf("exit %d:\n%s\nwant 0 false starts, the spliced clip heard, and PASS", code, text)
	}
}

// TestAFalseStartFails fails if a phrase in the audio that should hold none
// is not counted as a false start, or the run passes with one.
func TestAFalseStartFails(t *testing.T) {
	root := t.TempDir()
	copyClip(t, "start.wav", filepath.Join(root, "audio", "chapter"))
	var out bytes.Buffer
	code := run([]string{"-audio", filepath.Join(root, "audio"), "-min-hours", "0", "-limit", "0.5"}, &out, &out)
	if code == 0 || !strings.Contains(out.String(), "false_starts=1 ") {
		t.Fatalf("exit %d:\n%s\nwant 1 false start and a failure", code, out.String())
	}
}

// TestTooLittleIsUnknown fails if a run on less audio than its verdict needs,
// or a cell of phrases with fewer clips than it needs, reads as a pass.
func TestTooLittleIsUnknown(t *testing.T) {
	root := t.TempDir()
	copyClip(t, "stop.wav", filepath.Join(root, "audio", "chapter"))
	copyClip(t, "start.wav", filepath.Join(root, "phrases", "person1-1m"))
	var out bytes.Buffer
	code := run([]string{"-audio", filepath.Join(root, "audio"), "-phrases", filepath.Join(root, "phrases")}, &out, &out)
	text := out.String()
	if code == 0 || !strings.HasSuffix(strings.TrimSpace(text), "UNKNOWN") || !strings.Contains(text, "cell=person1-1m clips=1 heard=1") {
		t.Fatalf("exit %d:\n%s\nwant the cell counted and UNKNOWN", code, text)
	}
}
