//go:build kws && linux

package local

import (
	"path/filepath"
	"strings"
	"testing"
)

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

// TestTheSpotterHearsOnlyTheStartPhrase fails if the real spotter misses
// "Wispr, start recording" or "Ghost Wispr, start recording" in a stored
// synthetic clip, or takes "Wispr, stop recording" for either. The clips are
// made by espeak-ng (testdata/README.md); none is a recording of a person.
func TestTheSpotterHearsOnlyTheStartPhrase(t *testing.T) {
	d, err := NewDetector()
	if err != nil {
		t.Fatalf("load the spotter: %v", err)
	}
	t.Cleanup(d.Close)
	for _, tc := range []struct {
		clip string
		want bool
	}{
		{"start.wav", true},
		{"ghost-start.wav", true},
		{"stop.wav", false},
	} {
		if got := heardIn(t, d, tc.clip); got != tc.want {
			t.Errorf("%s: heard %t, want %t", tc.clip, got, tc.want)
		}
		d.Reset()
	}
}

// TestStageTwoReadsStopAsStop fails if the real recognizer, stage 2, writes
// "Wispr, stop recording" with startMark, or "Wispr, start recording"
// without it: stage 2 is what keeps a spoken stop from starting a recording
// once stage 1 takes it for the start phrase.
func TestStageTwoReadsStopAsStop(t *testing.T) {
	d, err := NewDetector()
	if err != nil {
		t.Fatalf("load the spotter: %v", err)
	}
	t.Cleanup(d.Close)
	for _, tc := range []struct {
		clip      string
		wantStart bool
	}{
		{"start.wav", true},
		{"stop.wav", false},
	} {
		pcm, err := ReadWAV(filepath.Join("testdata", tc.clip))
		if err != nil {
			t.Fatal(err)
		}
		samples := make([]float32, len(pcm)+tailSamples)
		for i, s := range pcm {
			samples[i] = float32(s) / 32768
		}
		text := d.verify.Text(samples)
		if got := strings.Contains(text, startMark); got != tc.wantStart {
			t.Errorf("stage 2 read %s as %q: holds %q %t, want %t", tc.clip, text, startMark, got, tc.wantStart)
		}
	}
}
