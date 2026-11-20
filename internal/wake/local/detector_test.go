package local

import (
	"slices"
	"testing"
)

// fakeSpotter is stage 1: it reports a start phrase at the sample counts in
// hits, counted from its last Reset.
type fakeSpotter struct {
	hits   []int
	fed    int
	resets int
}

func (s *fakeSpotter) Accept(samples []float32) bool {
	before := s.fed
	s.fed += len(samples)
	for _, at := range s.hits {
		if before < at && at <= s.fed {
			return true
		}
	}
	return false
}

func (s *fakeSpotter) Reset() { s.fed, s.resets = 0, s.resets+1 }
func (s *fakeSpotter) Close() {}

// fakeVerifier is stage 2: it says text, and keeps the samples it was given.
type fakeVerifier struct {
	text  string
	given [][]float32
}

func (v *fakeVerifier) Text(samples []float32) string {
	v.given = append(v.given, slices.Clone(samples))
	return v.text
}

func (v *fakeVerifier) Close() {}

// samplesOf returns n samples that each hold their index, offset by from, so a
// test can tell which audio reached stage 2.
func samplesOf(from, n int) []int16 {
	pcm := make([]int16, n)
	for i := range pcm {
		pcm[i] = int16((from + i) % 30000)
	}
	return pcm
}

// TestRingHoldsAtMostTwoSeconds fails if the detector keeps more than 2 s of
// the microphone's audio: it is fed 60 s, and stage 2, which reads the ring,
// must get the last 2 s and no sample older than that, padded with silence.
func TestRingHoldsAtMostTwoSeconds(t *testing.T) {
	const fed = 60 * SampleRate
	spot := &fakeSpotter{hits: []int{fed}}
	verify := &fakeVerifier{text: "WHISPER START RECORDING"}
	d := NewDetectorOf(spot, verify)
	for at := 0; at < fed; at += SampleRate / 4 {
		heard := d.Write(samplesOf(at, SampleRate/4))
		if at+SampleRate/4 < fed && heard {
			t.Fatalf("heard at %d samples, before the spotter's hit", at)
		}
	}
	if got := len(d.ring); got > RingSamples || RingSamples != 2*SampleRate {
		t.Fatalf("ring of %d samples (bound %d), want at most 2 s at %d Hz", got, RingSamples, SampleRate)
	}
	if len(verify.given) != 1 {
		t.Fatalf("stage 2 ran %d times, want once", len(verify.given))
	}
	given := verify.given[0]
	audio := given[:len(given)-tailSamples]
	if len(audio) != RingSamples {
		t.Fatalf("stage 2 got %d samples of audio, want the ring's %d", len(audio), RingSamples)
	}
	want := samplesOf(fed-RingSamples, RingSamples)
	for i, s := range audio {
		if got := int16(s * 32768); got != want[i] {
			t.Fatalf("stage 2's sample %d = %d, want %d: the ring held audio older than 2 s or out of order", i, got, want[i])
		}
	}
	for i, s := range given[len(given)-tailSamples:] {
		if s != 0 {
			t.Fatalf("stage 2's padding sample %d = %v, want silence", i, s)
		}
	}
}

// TestStageTwoAcceptsOnlyStart fails if a stage-1 hit counts without stage 2's
// text holding "STAR", as for "Wispr, stop recording", which stage 1 alone
// takes for the start phrase 6 times in 80 (gap-GapKWS.md §3.2).
func TestStageTwoAcceptsOnlyStart(t *testing.T) {
	for _, tc := range []struct {
		text string
		want bool
	}{
		{"WHISPER START RECORDING", true},
		{"COAST WHISPER STAR RECORDING", true},
		{"WHISPER STOP RECORDING", false},
		{"", false},
	} {
		spot := &fakeSpotter{hits: []int{SampleRate}}
		d := NewDetectorOf(spot, &fakeVerifier{text: tc.text})
		if got := d.Write(samplesOf(0, SampleRate)); got != tc.want {
			t.Errorf("stage 2 text %q: heard %t, want %t", tc.text, got, tc.want)
		}
	}
}

// TestStageTwoChecksTheDetectorsMark fails if stage 2 checks "STAR" in a
// detector built for another keyword, as the false-wake measurement's control
// is (cmd/measure-wake -control-keyword): its hits must count when stage 2's
// text holds that keyword, and only then.
func TestStageTwoChecksTheDetectorsMark(t *testing.T) {
	for _, tc := range []struct {
		mark, text string
		want       bool
	}{
		{"AND THEN", "SO AND THEN WE WENT", true},
		{"AND THEN", "WHISPER START RECORDING", false},
		{"", "WHISPER START RECORDING", true},
		{"", "SO AND THEN WE WENT", false},
	} {
		d := NewDetectorOf(&fakeSpotter{hits: []int{SampleRate}}, &fakeVerifier{text: tc.text})
		if tc.mark != "" {
			d.mark = tc.mark
		}
		if got := d.Write(samplesOf(0, SampleRate)); got != tc.want {
			t.Errorf("mark %q, stage 2 text %q: heard %t, want %t", tc.mark, tc.text, got, tc.want)
		}
	}
}

// TestEveryHitZeroesTheRing fails if a stage-1 hit, accepted or not, leaves
// audio in the ring or the spotter's stream: the next hit must not re-read it.
func TestEveryHitZeroesTheRing(t *testing.T) {
	for _, text := range []string{"WHISPER START RECORDING", "WHISPER STOP RECORDING"} {
		spot := &fakeSpotter{hits: []int{SampleRate}}
		d := NewDetectorOf(spot, &fakeVerifier{text: text})
		d.Write(samplesOf(1, SampleRate))
		if !allZero(d) {
			t.Errorf("after a hit whose stage 2 said %q, the detector still holds audio", text)
		}
		if spot.resets != 1 {
			t.Errorf("after a hit whose stage 2 said %q, the spotter was reset %d times, want 1", text, spot.resets)
		}
	}
}

// allZero reports whether every sample the detector holds, in its ring and in
// its conversion buffer, is zero.
func allZero(d *Detector) bool {
	for _, s := range d.ring[:] {
		if s != 0 {
			return false
		}
	}
	for _, s := range d.scratch[:cap(d.scratch)] {
		if s != 0 {
			return false
		}
	}
	return d.filled == 0
}
