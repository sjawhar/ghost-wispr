package voice

import (
	"bytes"
	"flag"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/loudness"
)

// speechLoudness is the loudness of Wispr's speech, in LUFS: the median
// BS.1770 short-term (3 s) loudness of voice A's speech, Gemini
// gemini-3.8-flash-tts with the voice Kore, saying the ten sentences of
// internal/voice/tts's live test end to end (36.8 s). ffmpeg 6.1.1's ebur128
// filter read -15.0 LUFS over the 339 windows wholly inside the speech, from
// -17.0 to -12.6, and an integrated loudness of -14.3 LUFS.
const speechLoudness = -15.0

// playedSounds are the sounds as Wispr plays them.
func playedSounds() map[string][]byte {
	return map[string][]byte{"wake": WakeChime(), "answer": AnswerChime(), "start": StartChime(), "error": ErrorChime()}
}

// TestChimesAreShortAndDistinct fails if the answer sound lasts less than
// 600 ms or more than 800 ms or is not one rising run of notes played twice,
// another sound lasts more than 400 ms, a sound clicks at its ends, is empty,
// has another number of notes than its design, or has no bounds here.
// TestChimesAreAsLoudAsSpeech checks that each can be heard.
func TestChimesAreShortAndDistinct(t *testing.T) {
	bounds := map[string]struct {
		notes    int
		shortest time.Duration
		longest  time.Duration
	}{
		"wake":   {2, 0, 400 * time.Millisecond},
		"answer": {6, 600 * time.Millisecond, 800 * time.Millisecond},
		"start":  {1, 0, 400 * time.Millisecond},
		"error":  {2, 0, 400 * time.Millisecond},
	}
	played := playedSounds()
	for name, notes := range chimeNotes {
		b, ok := bounds[name]
		if !ok {
			t.Errorf("the %s sound has no bounds in this test", name)
			continue
		}
		x := loudness.Samples(played[name])
		if len(x) == 0 {
			t.Fatalf("%s chime is empty, or not among playedSounds", name)
		}
		d := time.Duration(len(x)) * time.Second / time.Duration(ChimeFormat.SampleRate)
		if d < b.shortest || d > b.longest {
			t.Errorf("%s chime lasts %v, want %v to %v", name, d, b.shortest, b.longest)
		}
		const click = 200.0 / 32768 // of full scale
		if math.Abs(x[0]) > click || math.Abs(x[len(x)-1]) > click {
			t.Errorf("%s chime starts at %.4f and ends at %.4f of full scale, want near 0 (no click)", name, x[0], x[len(x)-1])
		}
		if len(notes) != b.notes {
			t.Errorf("%s chime has %d notes, want %d", name, len(notes), b.notes)
		}
	}
	answer := chimeNotes["answer"]
	run := answer[:len(answer)/2]
	rising := true
	for i := 1; i < len(run); i++ {
		rising = rising && run[i].hz > run[i-1].hz
	}
	if !rising || !slices.Equal(run, answer[len(answer)/2:]) {
		t.Errorf("the answer sound plays %v, want one rising run of notes twice", answer)
	}
}

// TestChimesAreAsLoudAsSpeech fails if a sound Wispr plays is, while it
// plays, more than 2 LU softer or louder than Wispr's speech, or its true peak
// is over -1 dBTP. A sound's loudness is BS.1770's over its own length.
// Short-term loudness would misread it: its 3 s window averages a sound of
// 300 ms with 2.7 s of silence, about 10 LU under what the room hears.
func TestChimesAreAsLoudAsSpeech(t *testing.T) {
	rate := ChimeFormat.SampleRate
	played := playedSounds()
	for name := range chimeNotes {
		pcm, ok := played[name]
		if !ok {
			t.Errorf("the %s sound is not among playedSounds", name)
			continue
		}
		x := loudness.Samples(pcm)
		l, peak := loudness.Integrated(x, rate), loudness.TruePeak(x, rate)
		t.Logf("the %s sound: %.1f LUFS over its %d ms, true peak %.1f dBTP", name, l, len(x)*1000/rate, peak)
		if !(math.Abs(l-speechLoudness) <= 2) { // a NaN fails too
			t.Errorf("the %s sound is %.1f LUFS while it plays, want within 2 LU of speech's %.1f", name, l, speechLoudness)
		}
		if !(peak <= -1) {
			t.Errorf("the %s sound peaks at %.1f dBTP, want -1 or lower", name, peak)
		}
	}
}

// updateSounds rewrites the speaker mixer's test copies of the sounds.
var updateSounds = flag.Bool("update-sounds", false, "rewrite internal/audio/testdata/sounds from the sounds Wispr plays")

// TestMixerTestsHaveTheSounds fails if internal/audio's test copies of the
// sounds, which its mixer tests play over Wispr's speech, are not exactly the
// sounds Wispr plays, one file for each. After a change to the sounds, run
// go test ./internal/voice -run TestMixerTestsHaveTheSounds -update-sounds.
func TestMixerTestsHaveTheSounds(t *testing.T) {
	dir := filepath.Join("..", "audio", "testdata", "sounds")
	played := playedSounds()
	if *updateSounds {
		old, err := filepath.Glob(filepath.Join(dir, "*.pcm"))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range old {
			if err := os.Remove(f); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, pcm := range played {
			if err := os.WriteFile(filepath.Join(dir, name+".pcm"), pcm, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.pcm"))
	if err != nil {
		t.Fatal(err)
	}
	var copies []string
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".pcm")
		copies = append(copies, name)
		pcm, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(pcm, played[name]) {
			t.Errorf("%s is not the %s sound Wispr plays; run this test with -update-sounds", f, name)
		}
	}
	if want := slices.Sorted(maps.Keys(chimeNotes)); !slices.Equal(copies, want) {
		t.Errorf("%s holds copies of %v, want one of each sound %v; run this test with -update-sounds", dir, copies, want)
	}
}
