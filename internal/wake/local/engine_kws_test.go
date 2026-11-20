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

// TestAControlKeywordReplacesThePhrase fails if Options.Keywords and
// Options.Mark do not make the real detector listen for another keyword in
// place of the start phrase, as the false-wake measurement's control does: a
// detector for "start recording" alone, with stage 2 checking "START", hears
// every stored start clip, and a detector for "and then", which no clip says,
// hears none of them.
func TestAControlKeywordReplacesThePhrase(t *testing.T) {
	starts, _ := clips(t)
	for _, tc := range []struct {
		keywords, mark string
		want           bool
	}{
		{"▁START ▁RE C OR D ING :1.0 #0.25 @CONTROL\n", "START", true},
		{"▁AND ▁THEN :1.0 #0.25 @CONTROL\n", "AND THEN", false},
	} {
		d, err := NewDetectorWith(&Options{Keywords: tc.keywords, Mark: tc.mark})
		if err != nil {
			t.Fatalf("load the spotter with the control keyword %q: %v", tc.keywords, err)
		}
		for _, clip := range starts {
			d.Reset()
			if got := heardIn(t, d, clip); got != tc.want {
				t.Errorf("a detector for %q, stage 2 checking %q: %s heard %t, want %t", strings.TrimSpace(tc.keywords), tc.mark, clip, got, tc.want)
			}
		}
		d.Close()
	}
}

// TestAnUnknownTokenIsRefused fails if a keyword line with a token the model
// lacks reaches sherpa-onnx, which would end the whole process, instead of
// being refused with an error that names the token.
func TestAnUnknownTokenIsRefused(t *testing.T) {
	for _, o := range []Options{
		{Keywords: "▁AND ▁NOTATOKENGW :1.0 #0.25 @CONTROL\n"},
		{Decoys: "▁W H IS PER ▁NOTATOKENGW :1.0 #0.25 @DECOY_X\n"},
	} {
		d, err := NewDetectorWith(&o)
		if err == nil {
			d.Close()
			t.Fatalf("%+v loaded", o)
		}
		if !strings.Contains(err.Error(), "▁NOTATOKENGW") {
			t.Errorf("%+v: the error %q does not name the token", o, err)
		}
	}
	d, err := NewDetectorWith(&Options{Keywords: "▁AND ▁THEN :1.0 #0.25 @CONTROL\n", Mark: "AND THEN"})
	if err != nil {
		t.Fatalf("control: the measurement's default control keyword does not load: %v", err)
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
