//go:build kws && linux

package local

import (
	"path/filepath"
	"strings"
	"testing"
)

// clips lists the stored clips (testdata/README.md): the start phrases, which
// the listener must hear, and the near misses, which it must not.
func clips(t *testing.T) (starts, misses []string) {
	t.Helper()
	all, err := filepath.Glob(filepath.Join("testdata", "*.wav"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range all {
		name := filepath.Base(path)
		if strings.HasPrefix(name, "start-") || strings.HasPrefix(name, "ghost-start-") {
			starts = append(starts, name)
		} else {
			misses = append(misses, name)
		}
	}
	if len(starts) < 5 || len(misses) < 4 {
		t.Fatalf("UNKNOWN: testdata holds %d start clips and %d near misses, want 5 and 4", len(starts), len(misses))
	}
	return starts, misses
}

// heardIn feeds a stored clip to d in the microphone's 100 ms buffers, after
// a second of silence and followed by another, and reports whether d heard
// the start phrase.
func heardIn(t *testing.T, d *Detector, clip string) bool {
	t.Helper()
	pcm, err := ReadWAV(filepath.Join("testdata", clip))
	if err != nil {
		t.Fatal(err)
	}
	silence := make([]int16, SampleRate)
	audio := append(append(append([]int16{}, silence...), pcm...), silence...)
	heard := false
	for len(audio) > 0 {
		n := min(len(audio), SampleRate/10)
		heard = d.Write(audio[:n]) || heard
		audio = audio[n:]
	}
	return heard
}

// TestTheKeywordsLoadAgainstTheModel fails if the two keyword lines name a
// token the embedded model lacks, or the model does not load: sherpa-onnx
// would end the whole process on the Pi instead.
func TestTheKeywordsLoadAgainstTheModel(t *testing.T) {
	d, err := NewDetector()
	if err != nil {
		t.Fatalf("load the spotter: %v", err)
	}
	d.Close()
}

// TestTheSpotterHearsOnlyTheStartPhrase fails if the real detector misses
// "Wispr, start recording" or "Ghost Wispr, start recording" in a stored
// synthetic clip, in American and British voices, or takes a near miss for
// either: "Wispr, stop recording", "Ghost Wispr, stop recording" or "Crisper,
// start recording". None is a recording of a person (testdata/README.md).
func TestTheSpotterHearsOnlyTheStartPhrase(t *testing.T) {
	d, err := NewDetector()
	if err != nil {
		t.Fatalf("load the spotter: %v", err)
	}
	t.Cleanup(d.Close)
	starts, misses := clips(t)
	for _, clip := range starts {
		d.Reset()
		if !heardIn(t, d, clip) {
			t.Errorf("%s: not heard", clip)
		}
	}
	for _, clip := range misses {
		d.Reset()
		if heardIn(t, d, clip) {
			t.Errorf("%s: heard as the start phrase", clip)
		}
	}
}

// TestStageTwoRefusesTheStopsStageOneHears fails if stage 1 does not take the
// stored "stop recording" clips for the start phrase, so that they no longer
// test stage 2, or if stage 2 then lets any of them through: stage 2 is what
// keeps a spoken stop from starting a recording.
func TestStageTwoRefusesTheStopsStageOneHears(t *testing.T) {
	d, err := NewDetector()
	if err != nil {
		t.Fatalf("load the spotter: %v", err)
	}
	t.Cleanup(d.Close)
	_, misses := clips(t)
	stops := 0
	for _, clip := range misses {
		if !strings.HasPrefix(clip, "stop-") {
			continue
		}
		stops++
		d.Reset()
		before := d.StageOneHits()
		heard := heardIn(t, d, clip)
		if d.StageOneHits() == before {
			t.Errorf("control: stage 1 did not fire on %s, so it does not test stage 2", clip)
		}
		if heard {
			t.Errorf("%s: stage 2 let the stop through", clip)
		}
	}
	if stops < 3 {
		t.Fatalf("UNKNOWN: %d stop clips, want 3", stops)
	}
}
