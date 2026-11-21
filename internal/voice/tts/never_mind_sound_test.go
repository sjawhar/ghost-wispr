package tts_test

import (
	"context"
	"encoding/binary"
	"math"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/voice"
	"github.com/sjawhar/ghost-wispr/internal/voice/tts"
)

// recordingStream is a speaker stream at 24 kHz that keeps every sample it
// plays, takes each 10 ms frame in about real time, and holds none: Drop takes
// nothing back. It keeps how many samples it had played at each Drop, which
// the speaker calls when a Cancel takes effect, before it mixes the next
// frame: every sample from that index on was mixed after the cut.
type recordingStream struct {
	mu      sync.Mutex
	samples []int16
	drops   []int
}

func (s *recordingStream) Write(p []int16) error {
	s.mu.Lock()
	s.samples = append(s.samples, p...)
	s.mu.Unlock()
	time.Sleep(10 * time.Millisecond)
	return nil
}

func (s *recordingStream) Drop() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.drops = append(s.drops, len(s.samples))
	return 0, nil
}

func (*recordingStream) Close() error { return nil }

func (s *recordingStream) played() []int16 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.samples)
}

// cuts returns the stream position of each Drop so far.
func (s *recordingStream) cuts() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.drops)
}

// onOffLight is a waiting light that keeps whether it is on.
type onOffLight struct {
	mu sync.Mutex
	on bool
}

func (l *onOffLight) On()  { l.set(true) }
func (l *onOffLight) Off() { l.set(false) }

func (l *onOffLight) set(on bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.on = on
}

func (l *onOffLight) lit() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.on
}

// TestNeverMindCutsTheAnswerSoundOnTheRealSpeaker uses the real Supervisor
// and the real speaker output, which ducks speech on the voice input 15 dB
// under a sound on the chime input. "Wispr, never mind" drops the only held
// answer while its sound (the rising triad, twice, 660 ms) plays. The test
// finds the cut where the speaker took it: the stream position at which the
// speaker dropped the sound (recordingStream.Drop), never a sample value. It
// fails if the cut does not reach the speaker, or anything of the sound
// plays from the cut on; or, when speech started on the voice input under
// the sound, if that speech is cut, or from the cut on does not rise from
// its ducked level back to its own within one 10 ms frame, by at most the
// step a 9 ms release allows from one sample to the next, and stay there: a
// faster rise is a click.
func TestNeverMindCutsTheAnswerSoundOnTheRealSpeaker(t *testing.T) {
	const rate = 24000
	const speechLevel = 8000
	const frame = rate / 100 // one 10 ms frame of the speaker
	// The speech's level under the sound, 15 dB down, and the most it may rise
	// from one sample to the next on its way back: its gain takes 9 ms or more
	// to come back, as the speaker's ramp tests bound it (internal/audio
	// TestOutputDucksTheVoiceUnderASound).
	ducked := speechLevel * math.Pow(10, -15.0/20)
	step := (speechLevel - ducked) / float64(9*rate/1000)
	for _, tc := range []struct {
		name   string
		speech bool
		// cutAt is how far the speaker has played when the never mind is
		// said; 0 says it as soon as the test is ready.
		cutAt time.Duration
	}{
		{name: "the sound alone"},
		{name: "over speech that started under it", speech: true},
		// Scheduling can let the speaker run far into the sound before the
		// never mind: here, to 550 ms, 110 ms before the sound's end.
		{name: "over speech, the speaker 550 ms into the sound", speech: true, cutAt: 550 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := &recordingStream{}
			out := audio.NewOutput(audio.OutputConfig{Open: func(string) (audio.PlaybackStream, int, error) {
				return stream, rate, nil
			}})
			t.Cleanup(func() { _ = out.Close() })
			light := &onOffLight{}
			c := &heldOnly{answers: make(chan voice.PendingAnswer, 1), spoken: make(chan string, 1)}
			s := voice.NewSupervisor(&voice.SupervisorConfig{
				Voice:        func() (voice.Voice, error) { return tts.New(solTailEngine{}), nil },
				Concierge:    c,
				Speaker:      voice.SpeakerOf(out),
				Session:      func() string { return "sess-1" },
				WakePhrases:  []string{"Wispr"},
				Red:          func() bool { return true },
				SilenceWait:  500 * time.Millisecond,
				SoundGap:     10 * time.Millisecond,
				WaitingLight: light,
				Logf:         t.Logf,
			})
			t.Cleanup(s.Close)
			// No asked time: the answer waits for "Wispr, go ahead", so it
			// sounds and does not speak.
			c.answers <- voice.PendingAnswer{TaskID: "t1", SessionID: "sess-1", Text: "It ships tomorrow."}
			waitPlayed(t, stream, "the answer sound", func(got []int16) bool { return countNonZero(got) >= rate/20 })

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			t.Cleanup(cancel)
			spoken := make(chan error, 1)
			speechFrom := 0
			if tc.speech {
				pcm := make([]byte, 2*rate) // 1 s
				for i := 0; i < len(pcm); i += 2 {
					binary.LittleEndian.PutUint16(pcm[i:], speechLevel)
				}
				pb := out.Begin(audio.InputVoice, nil)
				speechFrom = len(stream.played())
				go func() {
					err := pb.WriteAll(ctx, pcm, audio.AudioFormat{SampleRate: rate, Channels: 1, Encoding: audio.EncodingPCM16})
					if err == nil {
						err = pb.Drain(ctx)
					}
					spoken <- err
				}()
				waitPlayed(t, stream, "50 ms of the speech under the sound", func(got []int16) bool { return len(got) >= speechFrom+rate/20 })
			}
			if tc.cutAt > 0 {
				at := int(tc.cutAt.Milliseconds()) * rate / 1000
				waitPlayed(t, stream, "the speaker at "+tc.cutAt.String(), func(got []int16) bool { return len(got) >= at })
			}
			if cuts := stream.cuts(); len(cuts) > 0 {
				t.Fatalf("the speaker dropped audio at %v before the never mind", cuts)
			}

			said := len(stream.played())
			words := []voice.Word{{Text: "Wispr,", Start: 1, End: 1.15}, {Text: "never", Start: 1.2, End: 1.35}, {Text: "mind.", Start: 1.4, End: 1.55}}
			s.OnTranscript(&voice.Transcript{Text: "Wispr, never mind.", Words: words, IsFinal: true, SpeechFinal: true, GateOpen: true, At: time.Now()})
			deadline := time.Now().Add(3 * time.Second)
			for light.lit() || len(stream.cuts()) == 0 {
				if time.Now().After(deadline) {
					t.Fatalf("after the never mind: waiting light on %v, the speaker's drops %v; want the answer dropped and its sound cut", light.lit(), stream.cuts())
				}
				time.Sleep(time.Millisecond)
			}
			if tc.speech {
				if err := <-spoken; err != nil {
					t.Fatalf("the speech under the sound = %v, want it played whole", err)
				}
			} else {
				time.Sleep(300 * time.Millisecond)
			}

			got, cuts := stream.played(), stream.cuts()
			if len(cuts) != 1 || cuts[0] < said || cuts[0] < frame {
				t.Fatalf("the speaker dropped audio at %v, want once, the cut, after the %d samples played when the never mind was said", cuts, said)
			}
			cut := cuts[0]
			// Control: the sound still played in the frame before the cut, so
			// the cut stopped it rather than its end, and the frame holds more
			// than the speech ducked under it.
			alone := int16(0)
			if tc.speech {
				alone = int16(math.Round(ducked))
			}
			if !slices.ContainsFunc(got[cut-frame:cut], func(v int16) bool { return v != alone }) {
				t.Fatalf("the frame before the cut at sample %d holds no sound: it had ended, and the cut is not tested", cut)
			}
			after := got[cut:]
			if !tc.speech {
				if n := countNonZero(after); n > 0 {
					t.Fatalf("%d samples of the answer sound played from the cut at sample %d", n, cut)
				}
				return
			}
			// Control: the speech played ducked under the sound before the cut.
			under := 0
			for _, v := range got[speechFrom:cut] {
				if v != 0 && v != speechLevel {
					under++
				}
			}
			if under < rate/50 {
				t.Fatalf("only %d samples of the speech played under the sound before the cut, want 20 ms or more", under)
			}
			// From the cut on only the speech plays, until it ends: it rises
			// from its ducked level to its own, then stays there.
			if end := slices.Index(after, 0); end >= 0 {
				after = after[:end]
			}
			if len(after) <= frame {
				t.Fatalf("the speech played %d samples from the cut, want its release and more", len(after))
			}
			prev, full := ducked, -1
			for i, v := range after {
				if d := float64(v) - prev; d < 0 || d > step || v > speechLevel {
					t.Fatalf("sample %d from the cut (stream sample %d) is %d, after %.0f: the speech must rise from its ducked level %.0f to %d by at most %.1f a sample, a release of 9 ms or more; a bigger rise is a click", i, cut+i, v, prev, ducked, speechLevel, step)
				}
				if v == speechLevel && full < 0 {
					full = i
				}
				prev = float64(v)
			}
			if full < 0 || full >= frame {
				t.Fatalf("the speech came back to %d at sample %d from the cut, want within one %d-sample frame", speechLevel, full, frame)
			}
		})
	}
}

func waitPlayed(t *testing.T, stream *recordingStream, what string, done func([]int16) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !done(stream.played()) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func countNonZero(samples []int16) int {
	n := 0
	for _, v := range samples {
		if v != 0 {
			n++
		}
	}
	return n
}
