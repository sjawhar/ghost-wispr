package voice

import (
	"encoding/binary"
	"math"
	"sync"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

// ChimeFormat is the format of both chimes: the voice output's mix rate.
var ChimeFormat = audio.AudioFormat{SampleRate: 24000, Channels: 1, Encoding: audio.EncodingPCM16}

// note is one tone of a chime: a sine with a little of its octave, a 5 ms
// attack and an exponential decay, so it rings like a small bell.
type note struct {
	hz  float64
	ms  int
	amp float64 // of full scale
}

// chimeNotes are the sounds (R10). The wake sound rises a fifth over two
// notes (E5, B5): "I heard you". The answer sound is three notes up a major
// triad (G5, C6, E6): "an answer is ready". The hand sound is two short taps
// on one high note (C6, 8 semitones over the wake sound's start): "may I?".
// The start sound is one soft high note (A5): "recording". The error sound
// falls a fourth over two notes from A4, 7 semitones under the wake sound's
// start: "that did not work". Sounds with as many notes start at least 4
// semitones apart.
var chimeNotes = map[string][]note{
	"wake":   {{659.25, 90, 0.30}, {987.77, 150, 0.26}},
	"answer": {{783.99, 80, 0.26}, {1046.50, 80, 0.26}, {1318.51, 170, 0.22}},
	"hand":   {{1046.50, 110, 0.24}, {1046.50, 170, 0.24}},
	"start":  {{880.00, 220, 0.26}},
	"error":  {{440.00, 130, 0.32}, {329.63, 200, 0.32}},
}

var (
	wakeChime   = sync.OnceValue(func() []byte { return renderChime(chimeNotes["wake"]) })
	answerChime = sync.OnceValue(func() []byte { return renderChime(chimeNotes["answer"]) })
	handChime   = sync.OnceValue(func() []byte { return renderChime(chimeNotes["hand"]) })
	startChime  = sync.OnceValue(func() []byte { return renderChime(chimeNotes["start"]) })
	errorChime  = sync.OnceValue(func() []byte { return renderChime(chimeNotes["error"]) })
)

// WakeChime is the sound Wispr plays when it hears its name, PCM16 LE in
// ChimeFormat. Callers must not change it.
func WakeChime() []byte { return wakeChime() }

// AnswerChime is the sound Wispr plays when a holder's answer is ready,
// PCM16 LE in ChimeFormat. Callers must not change it.
func AnswerChime() []byte { return answerChime() }

// HandChime is the sound Wispr plays when a joined agent asks for the floor,
// PCM16 LE in ChimeFormat. Callers must not change it.
func HandChime() []byte { return handChime() }

// StartChime is the sound Wispr plays when capture moves into recording.
func StartChime() []byte { return startChime() }

// ErrorChime is the sound Wispr plays when it cannot do what was asked: the
// Concierge cannot reach Dispatch or Envoy, a voice fails, or a red check
// fails.
func ErrorChime() []byte { return errorChime() }

// renderChime renders notes one after the other; each note keeps ringing
// under the next for its decay, and the whole fades to silence at the end.
func renderChime(notes []note) []byte {
	rate := float64(ChimeFormat.SampleRate)
	total := 0
	for _, n := range notes {
		total += n.ms
	}
	samples := int(float64(total) * rate / 1000)
	mix := make([]float64, samples)
	offset := 0
	for _, n := range notes {
		start := offset
		offset += int(float64(n.ms) * rate / 1000)
		for i := start; i < samples; i++ {
			t := float64(i-start) / rate
			attack := min(1, t/0.005)
			decay := math.Exp(-t / 0.09)
			phase := 2 * math.Pi * n.hz * t
			mix[i] += n.amp * attack * decay * (0.85*math.Sin(phase) + 0.15*math.Sin(2*phase))
		}
	}
	out := make([]byte, 2*samples)
	fade := int(0.02 * rate) // the last 20 ms fade to 0, so the end does not click
	for i, s := range mix {
		if left := samples - 1 - i; left < fade {
			s *= float64(left) / float64(fade)
		}
		s = max(-1, min(1, s))
		binary.LittleEndian.PutUint16(out[2*i:], uint16(int16(s*math.MaxInt16)))
	}
	return out
}
