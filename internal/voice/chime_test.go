package voice

import (
	"encoding/binary"
	"math"
	"testing"
	"time"
)

// TestChimesAreShortSoftAndDistinct fails if a sound lasts more than 400 ms,
// clips, clicks at its ends, is silent, or has another number of notes than
// its design.
func TestChimesAreShortSoftAndDistinct(t *testing.T) {
	chimes := map[string]struct {
		pcm   []byte
		notes int
	}{
		"wake":   {WakeChime(), 2},
		"answer": {AnswerChime(), 3},
		"hand":   {HandChime(), 2},
		"start":  {StartChime(), 1},
		"error":  {ErrorChime(), 2},
	}
	for name, c := range chimes {
		samples := len(c.pcm) / 2
		if samples == 0 {
			t.Fatalf("%s chime is empty", name)
		}
		d := time.Duration(samples) * time.Second / time.Duration(ChimeFormat.SampleRate)
		if d > 400*time.Millisecond {
			t.Errorf("%s chime lasts %v, want at most 400 ms", name, d)
		}
		var peak float64
		at := func(i int) float64 { return float64(int16(binary.LittleEndian.Uint16(c.pcm[2*i:]))) }
		for i := range samples {
			peak = max(peak, math.Abs(at(i)))
		}
		if peak < 3000 || peak > 20000 {
			t.Errorf("%s chime peaks at %.0f, want a soft but audible 3000 to 20000", name, peak)
		}
		if math.Abs(at(0)) > 200 || math.Abs(at(samples-1)) > 200 {
			t.Errorf("%s chime starts at %.0f and ends at %.0f, want near 0 (no click)", name, at(0), at(samples-1))
		}
		if got := len(chimeNotes[name]); got != c.notes {
			t.Errorf("%s chime has %d notes, want %d", name, got, c.notes)
		}
	}
}
