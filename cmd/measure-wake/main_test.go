package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/wake/local"
)

// The tests run the tool's counting on a fake spotter, so they read what the
// tool counts, not how well the model hears: the real spotter's own tests are
// internal/wake/local's, and its accuracy is what this tool measures.

// phraseSample marks a test clip's start phrase: the fake spotter hears a
// buffer that holds it.
const phraseSample = 4242

// nearSample marks speech that sounds like the phrase: only a permissive
// stage 1, the control's (boost 2 or more), hears it.
const nearSample = 4243

// phraseSeconds is a test phrase's length: one 100 ms buffer, which the tool
// feeds at a stream's own 100 ms steps, so that the fake spotter hears it
// once.
const phraseSeconds = 0.1

type markSpotter struct{ permissive bool }

func (s markSpotter) Accept(samples []float32) bool {
	return slices.Contains(samples, float32(phraseSample)/32768) ||
		(s.permissive && slices.Contains(samples, float32(nearSample)/32768))
}
func (markSpotter) Reset() {}
func (markSpotter) Close() {}

// textVerifier is stage 2: it says text.
type textVerifier string

func (v textVerifier) Text([]float32) string { return string(v) }
func (textVerifier) Close()                  {}

// fakeDetector is the tool's detector on the fake stages; stage 2 refuses
// every hit when -stage2 is on and refuse is set, and a boost of 2 or more
// makes stage 1 hear near misses too.
func fakeDetector(refuse bool) func(*options) (*local.Detector, error) {
	return func(o *options) (*local.Detector, error) {
		text := textVerifier("WHISPER START RECORDING")
		if refuse && o.stage2 {
			text = "WHISPER STOP RECORDING"
		}
		return local.NewDetectorOf(markSpotter{permissive: o.boost >= 2}, text), nil
	}
}

// writeWAV writes seconds of 16 kHz mono PCM16 to dir/name, every sample
// value, so that value phraseSample is a start phrase and any other is speech.
func writeWAV(t *testing.T, dir, name string, seconds float64, value int16) {
	t.Helper()
	samples := int(seconds * local.SampleRate)
	wav := make([]byte, 44+2*samples)
	copy(wav, "RIFF")
	binary.LittleEndian.PutUint32(wav[4:], uint32(36+2*samples))
	copy(wav[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(wav[16:], 16)
	binary.LittleEndian.PutUint16(wav[20:], 1)
	binary.LittleEndian.PutUint16(wav[22:], 1)
	binary.LittleEndian.PutUint32(wav[24:], local.SampleRate)
	binary.LittleEndian.PutUint32(wav[28:], 2*local.SampleRate)
	binary.LittleEndian.PutUint16(wav[32:], 2)
	binary.LittleEndian.PutUint16(wav[34:], 16)
	copy(wav[36:], "data")
	binary.LittleEndian.PutUint32(wav[40:], uint32(2*samples))
	for i := range samples {
		binary.LittleEndian.PutUint16(wav[44+2*i:], uint16(value))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), wav, 0o644); err != nil {
		t.Fatal(err)
	}
}

func runTool(t *testing.T, refuse bool, args ...string) (int, string) {
	t.Helper()
	var out bytes.Buffer
	code := run(args, &out, &out, fakeDetector(refuse))
	return code, out.String()
}

func requireOutput(t *testing.T, code int, text string, wantCode int, want ...string) {
	t.Helper()
	if code != wantCode {
		t.Fatalf("exit %d, want %d:\n%s", code, wantCode, text)
	}
	for _, line := range want {
		if !strings.Contains(text, line) {
			t.Fatalf("output lacks %q:\n%s", line, text)
		}
	}
}

// speechWithNearMiss writes a stream of speech that holds one stretch which
// sounds like the phrase: the control hears it, and the listener must not.
func speechWithNearMiss(t *testing.T, dir string) {
	t.Helper()
	writeWAV(t, dir, "1-speech.wav", 2, 100)
	writeWAV(t, dir, "2-near.wav", phraseSeconds, nearSample)
	writeWAV(t, dir, "3-speech.wav", 1, 100)
}

// TestSplicedPhrasesAreNotFalseStarts fails if the run counts a phrase it
// spliced in as a control as a false start, misses it, or counts a false start
// in speech with no phrase, or if a run whose two controls both fired does not
// pass.
func TestSplicedPhrasesAreNotFalseStarts(t *testing.T) {
	root := t.TempDir()
	speechWithNearMiss(t, filepath.Join(root, "audio", "chapter"))
	writeWAV(t, filepath.Join(root, "splice"), "start.wav", phraseSeconds, phraseSample)
	code, text := runTool(t, false, "-audio", filepath.Join(root, "audio"), "-splice", filepath.Join(root, "splice"), "-splice-every", "1s", "-min-hours", "0")
	requireOutput(t, code, text, 0, "false_starts=0 ", "spliced_clips=", "control_boost=3 control_threshold=0.01 control_false_starts=1", "PASS")
	if strings.Contains(text, "spliced_heard=0") {
		t.Fatalf("the spliced phrases were not heard:\n%s", text)
	}
}

// TestAFalseStartFails fails if a phrase in the audio that should hold none
// is not counted as a false start, or the run passes with one: a failing
// count fails with or without its controls.
func TestAFalseStartFails(t *testing.T) {
	root := t.TempDir()
	writeWAV(t, filepath.Join(root, "audio", "chapter"), "1-speech.wav", 1, 100)
	writeWAV(t, filepath.Join(root, "audio", "chapter"), "2-start.wav", phraseSeconds, phraseSample)
	code, text := runTool(t, false, "-audio", filepath.Join(root, "audio"), "-min-hours", "0")
	requireOutput(t, code, text, 1, "false_starts=1 ", "stage1_hits_in_speech=1 ", "FAIL")
}

// TestStageTwoRefusalsAreCountedApart fails if a stage-1 hit that stage 2
// refuses counts as a false start, or goes uncounted as a stage-1 hit, or if a
// listener that refuses every phrase passes (review 5998292452 of #123): with
// no spliced phrase, nothing shows it can hear one, and with one, it refuses
// it too.
func TestStageTwoRefusalsAreCountedApart(t *testing.T) {
	root := t.TempDir()
	writeWAV(t, filepath.Join(root, "audio", "chapter"), "start.wav", phraseSeconds, phraseSample)
	writeWAV(t, filepath.Join(root, "splice"), "start.wav", phraseSeconds, phraseSample)
	code, text := runTool(t, true, "-audio", filepath.Join(root, "audio"), "-min-hours", "0")
	requireOutput(t, code, text, 1, "false_starts=0 ", "stage1_hits_in_speech=1 ", "UNKNOWN: no start phrase was spliced into the speech")
	code, text = runTool(t, true, "-audio", filepath.Join(root, "audio"), "-min-hours", "0", "-splice", filepath.Join(root, "splice"), "-splice-every", "1ms")
	requireOutput(t, code, text, 1, "false_starts=0 ", "spliced_heard=0", "UNKNOWN: the listener heard none of the spliced phrases")
	code, text = runTool(t, true, "-audio", filepath.Join(root, "audio"), "-min-hours", "0", "-stage2=false")
	requireOutput(t, code, text, 1, "false_starts=1 ", "stage1_hits_in_speech=1 ", "FAIL")
}

// TestTheControlMustFireToPass fails if 0 false starts passes when the
// control, the spotter at its most permissive on the same speech, also counts
// 0, or no more than the listener: the speech then holds too little that sounds
// like the phrase to show the listener rejects it (check c-listener-accuracy).
func TestTheControlMustFireToPass(t *testing.T) {
	root := t.TempDir()
	writeWAV(t, filepath.Join(root, "audio", "chapter"), "speech.wav", 3, 100)
	writeWAV(t, filepath.Join(root, "splice"), "start.wav", phraseSeconds, phraseSample)
	code, text := runTool(t, false, "-audio", filepath.Join(root, "audio"), "-splice", filepath.Join(root, "splice"), "-splice-every", "1s", "-min-hours", "0")
	requireOutput(t, code, text, 1, "false_starts=0 ", "control_false_starts=0", "UNKNOWN: the control counted no false start in this speech")

	writeWAV(t, filepath.Join(root, "audio2", "chapter"), "1-speech.wav", 2, 100)
	writeWAV(t, filepath.Join(root, "audio2", "chapter"), "2-start.wav", phraseSeconds, phraseSample)
	code, text = runTool(t, false, "-audio", filepath.Join(root, "audio2"), "-splice", filepath.Join(root, "splice"), "-splice-every", "1s", "-min-hours", "0", "-limit", "1e12")
	requireOutput(t, code, text, 1, "false_starts=1 ", "control_false_starts=1", "UNKNOWN: the control counted no more false starts than the listener")
}

// TestThePhrasesAreTheSpliceByDefault fails if a run given -phrases and no
// -splice, as the check's own command line is, splices no phrase into the
// speech, and so can never pass.
func TestThePhrasesAreTheSpliceByDefault(t *testing.T) {
	root := t.TempDir()
	speechWithNearMiss(t, filepath.Join(root, "audio", "chapter"))
	for i := range 10 {
		writeWAV(t, filepath.Join(root, "phrases", "person1-1m"), "start"+string(rune('a'+i))+".wav", phraseSeconds, phraseSample)
	}
	code, text := runTool(t, false, "-audio", filepath.Join(root, "audio"), "-phrases", filepath.Join(root, "phrases"), "-splice-every", "1s", "-min-hours", "0")
	requireOutput(t, code, text, 0, "spliced_clips=", "control_false_starts=1", "cell=person1-1m clips=10 heard=10", "PASS")
	if strings.Contains(text, "spliced_heard=0") {
		t.Fatalf("the phrases spliced in were not heard:\n%s", text)
	}
}

// TestTooLittleIsUnknown fails if a run on less audio than its verdict needs,
// or a cell of phrases with fewer clips than it needs, or a splice control that
// heard nothing, reads as a pass.
func TestTooLittleIsUnknown(t *testing.T) {
	root := t.TempDir()
	writeWAV(t, filepath.Join(root, "audio", "chapter"), "speech.wav", 1, 100)
	writeWAV(t, filepath.Join(root, "phrases", "person1-1m"), "start.wav", phraseSeconds, phraseSample)
	code, text := runTool(t, false, "-audio", filepath.Join(root, "audio"), "-phrases", filepath.Join(root, "phrases"))
	requireOutput(t, code, text, 1, "UNKNOWN: 0.000 h of audio, fewer than 10 h", "cell=person1-1m clips=1 heard=1", "UNKNOWN: cell person1-1m holds 1 clips, fewer than 10")
	if !strings.HasSuffix(strings.TrimSpace(text), "UNKNOWN") {
		t.Fatalf("verdict is not UNKNOWN:\n%s", text)
	}

	writeWAV(t, filepath.Join(root, "deaf"), "speech.wav", 0.5, 100) // a "phrase" the spotter cannot hear
	code, text = runTool(t, false, "-audio", filepath.Join(root, "audio"), "-splice", filepath.Join(root, "deaf"), "-splice-every", "1ms", "-min-hours", "0")
	requireOutput(t, code, text, 1, "spliced_heard=0", "UNKNOWN: the listener heard none of the spliced phrases")
}

// TestParallelDetectorsCountEveryStream fails if a run with -parallel 2
// drops or double-counts a stream: three streams, each with one phrase, give
// three false starts.
func TestParallelDetectorsCountEveryStream(t *testing.T) {
	root := t.TempDir()
	for _, stream := range []string{"one", "two", "three"} {
		writeWAV(t, filepath.Join(root, "audio", stream), "start.wav", phraseSeconds, phraseSample)
	}
	code, text := runTool(t, false, "-audio", filepath.Join(root, "audio"), "-min-hours", "0", "-parallel", "2")
	requireOutput(t, code, text, 1, "audio_files=3 ", "false_starts=3 ", "stage1_hits_in_speech=3 ")
}

// TestPhraseCellsAreCounted fails if a cell's hits are miscounted, or a hit
// rate under -min-hit-rate passes.
func TestPhraseCellsAreCounted(t *testing.T) {
	root := t.TempDir()
	for i := range 10 {
		writeWAV(t, filepath.Join(root, "phrases", "heard"), "start"+string(rune('a'+i))+".wav", phraseSeconds, phraseSample)
		writeWAV(t, filepath.Join(root, "phrases", "missed"), "start"+string(rune('a'+i))+".wav", 0.5, 100)
	}
	code, text := runTool(t, false, "-phrases", filepath.Join(root, "phrases"))
	requireOutput(t, code, text, 1, "cell=heard clips=10 heard=10", "cell=missed clips=10 heard=0", "phrases=20 heard=10 hit_rate=0.500", "FAIL")
}

// TestEachClipIsListed fails if -each does not name every clip of -phrases
// with whether stage 1 fired on it and whether the listener heard it: the
// lines a person reads to see which phrases were missed, and which stage-2
// refusals kept a near miss out.
func TestEachClipIsListed(t *testing.T) {
	root := t.TempDir()
	writeWAV(t, filepath.Join(root, "phrases", "cell"), "a.wav", phraseSeconds, phraseSample)
	writeWAV(t, filepath.Join(root, "phrases", "cell"), "b.wav", 0.5, 100)
	code, text := runTool(t, true, "-phrases", filepath.Join(root, "phrases"), "-each", "-min-cell", "1", "-min-hit-rate", "0")
	requireOutput(t, code, text, 0, "clip=cell/a.wav stage1=1 heard=false", "clip=cell/b.wav stage1=0 heard=false")
}
