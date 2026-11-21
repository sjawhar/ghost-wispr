package audio

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/loudness"
)

// testdata/speech.pcm is Wispr's speech: voice A as the office Pi runs it
// (Gemini gemini-3.8-flash-tts, the voice Kore, Vertex AI location us) saying
// the ten sentences of internal/voice/tts's live test end to end, 36.8 s of
// PCM16 LE mono at 24 kHz. speechSentences is how long each sentence lasts.
var speechSentences = []time.Duration{3800, 2920, 4080, 4200, 2800, 4240, 3600, 4400, 3200, 3600}

// testdata/sounds holds the sounds Wispr plays, PCM16 LE mono at 24 kHz;
// internal/voice's TestMixerTestsHaveTheSounds keeps them current.

// soundOverSpeech is a sound mixed over speech: the stream the mixer wrote,
// each input's part in it, and where in the stream each copy of the sound
// began.
type soundOverSpeech struct {
	stream        []int16
	speech, sound []float32
	soundAt       []int
}

// mixSoundOverSpeech plays speech on the voice input and a copy of sound on
// the chime input at each of starts, through the Output's mixer at 48 kHz, one
// 10 ms frame at a time, as a stream that never waits would take them.
func mixSoundOverSpeech(speech, sound []byte, starts []time.Duration) soundOverSpeech {
	const rate = 48000
	o := &Output{inputs: map[string]*outputInput{}, eventWake: make(chan struct{}, 1)}
	voicePb, chimePb := o.Begin(InputVoice, nil), o.Begin(InputChime, nil)
	voice, chime := o.inputs[InputVoice], o.inputs[InputChime]
	voice.setRate(24000)
	voice.ring = make([]int16, max(1, len(speech)/2))
	voice.push(voicePb, speech, 1, len(speech)/2)
	chime.setRate(24000)
	chime.ring = make([]int16, max(1, len(sound)/2*len(starts)))
	n := rate * int(outputFrame/time.Millisecond) / 1000
	frame, sum := make([]int16, n), make([]float32, n)
	// Byte counts of 24 kHz PCM16 are sample counts at 48 kHz.
	lastEnd := int(time.Duration(rate)*starts[len(starts)-1]/time.Second) + len(sound)
	length := max(len(speech), lastEnd) + rate/10
	var m soundOverSpeech
	next := 0
	for at := 0; at < length; at += n {
		if next < len(starts) && at >= int(time.Duration(rate)*starts[next]/time.Second) {
			chime.push(chimePb, sound, 1, len(sound)/2)
			m.soundAt = append(m.soundAt, -1)
			next++
		}
		o.mixFrame(frame, sum, rate, n, time.Unix(0, int64(at)*int64(time.Second)/rate))
		for i := range frame {
			if k := len(m.soundAt) - 1; k >= 0 && m.soundAt[k] < 0 && chime.contrib[i] != 0 {
				m.soundAt[k] = at + i
			}
		}
		m.stream = append(m.stream, frame...)
		m.speech = append(m.speech, voice.contrib...)
		m.sound = append(m.sound, chime.contrib...)
	}
	return m
}

func saturated(s int16) bool { return s == math.MaxInt16 || s == math.MinInt16 }

// TestSoundsOverSpeech fails if a sound Wispr plays while it speaks adds
// clipping to its speech, or is not clearly heard over it. Each sound plays
// 500 ms into each of the ten sentences of Wispr's speech (testdata), through
// the Output's mixer and resampler at 48 kHz. Every sample that saturates
// with the sound must saturate in the speech alone, and over each copy of the
// sound, the sound must be at least minSoundOverSpeechLU louder than the
// speech under it, by BS.1770 loudness over the sound's length. Played alone,
// the sound keeps its loudness.
func TestSoundsOverSpeech(t *testing.T) {
	const rate = 48000
	// minSoundOverSpeechLU is how much louder than the speech under it a sound
	// must be to be heard clearly: plainly the louder of the two, about half
	// again as loud. Not ducked, the sounds were 4.6 to 6.4 LU softer than the
	// loudest of this speech.
	const minSoundOverSpeechLU = 6
	speech, err := os.ReadFile(filepath.Join("testdata", "speech.pcm"))
	if err != nil {
		t.Fatal(err)
	}
	var starts []time.Duration
	var sentence time.Duration
	for _, d := range speechSentences {
		starts = append(starts, sentence+500*time.Millisecond)
		sentence += d * time.Millisecond
	}
	if want := time.Duration(len(speech)/2) * time.Second / 24000; sentence != want {
		t.Fatalf("the sentences last %v, the speech %v", sentence, want)
	}
	alone := mixSoundOverSpeech(speech, nil, starts)
	speechSaturated := 0
	for _, s := range alone.stream {
		if saturated(s) {
			speechSaturated++
		}
	}
	t.Logf("the speech alone saturates %d samples", speechSaturated)
	files, err := filepath.Glob(filepath.Join("testdata", "sounds", "*.pcm"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no sounds in testdata/sounds: %v", err)
	}
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".pcm")
		t.Run(name, func(t *testing.T) {
			sound, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			m := mixSoundOverSpeech(speech, sound, starts)
			total, added := 0, 0
			for i, s := range m.stream {
				if saturated(s) {
					total++
					if i >= len(alone.stream) || !saturated(alone.stream[i]) {
						added++
					}
				}
			}
			length := len(sound) / 2 * rate / 24000
			quietest, quietestAt := math.Inf(1), 0
			for k, at := range m.soundAt {
				if at < 0 {
					t.Fatalf("copy %d of the sound never played", k+1)
				}
				end := min(at+length, len(m.sound))
				over := loudness.Integrated(f64(m.sound[at:end]), rate) - loudness.Integrated(f64(m.speech[at:end]), rate)
				if over < quietest {
					quietest, quietestAt = over, k
				}
			}
			t.Logf("with the %s sound over the speech: %d saturated samples, %d of them not in the speech alone; the sound is at least %.1f LU louder than the speech under it (sentence %d)",
				name, total, added, quietest, quietestAt+1)
			if added > 0 {
				t.Errorf("the %s sound over the speech saturates %d samples that the speech alone does not (%d in all, %d in the speech alone)", name, added, total, speechSaturated)
			}
			if !(quietest >= minSoundOverSpeechLU) {
				t.Errorf("the %s sound is only %.1f LU louder than the speech under it in sentence %d, want at least %d", name, quietest, quietestAt+1, minSoundOverSpeechLU)
			}
			solo := mixSoundOverSpeech(nil, sound, starts[:1])
			at := solo.soundAt[0]
			got, want := loudness.Integrated(f64(solo.sound[at:at+length]), rate), loudness.Integrated(loudness.Samples(sound), 24000)
			if !(math.Abs(got-want) <= 0.2) {
				t.Errorf("the %s sound played alone is %.2f LUFS, want its own %.2f", name, got, want)
			}
		})
	}
}

// f64 is x as samples of full scale 1.
func f64(x []float32) []float64 {
	y := make([]float64, len(x))
	for i, v := range x {
		y[i] = float64(v) / 32768
	}
	return y
}
