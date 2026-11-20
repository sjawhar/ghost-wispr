package tts_test

import (
	"context"
	"encoding/binary"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/voice"
	"github.com/sjawhar/ghost-wispr/internal/voice/tts"
)

// recordingStream is a speaker stream at 24 kHz that keeps every sample it
// plays, and takes each 10 ms frame in about real time.
type recordingStream struct {
	mu      sync.Mutex
	samples []int16
}

func (s *recordingStream) Write(p []int16) error {
	s.mu.Lock()
	s.samples = append(s.samples, p...)
	s.mu.Unlock()
	time.Sleep(10 * time.Millisecond)
	return nil
}

func (*recordingStream) Drop() (int, error) { return 0, nil }
func (*recordingStream) Close() error       { return nil }

func (s *recordingStream) played() []int16 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int16(nil), s.samples...)
}

// lightAt records how many samples the stream had played when the waiting
// light went off: the moment the never mind dropped the last answer.
type lightAt struct {
	stream *recordingStream
	mu     sync.Mutex
	off    int
	offs   int
}

func (l *lightAt) On() {}

func (l *lightAt) Off() {
	n := len(l.stream.played())
	l.mu.Lock()
	defer l.mu.Unlock()
	l.off, l.offs = n, l.offs+1
}

func (l *lightAt) wentOff() (int, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.off, l.offs > 0
}

// TestNeverMindCutsTheAnswerSoundOnTheRealSpeaker uses the real Supervisor
// and the real speaker output, which ducks speech on the voice input 15 dB
// under a sound on the chime input. It fails if "Wispr, never mind", which
// drops the only held answer while its sound (the rising triad, twice, 660
// ms) plays, leaves any of that sound playing more than two 10 ms frames
// after the drop; or, when speech started on the voice input under the
// sound, if that speech is cut, or does not come back from ducked to its own
// level within 10 ms of the sound's end and stay there.
func TestNeverMindCutsTheAnswerSoundOnTheRealSpeaker(t *testing.T) {
	const rate = 24000
	const speechLevel = 8000
	const slack = 2 * rate / 100 // two 10 ms frames: one mixed before the cancel, one under way
	for _, speech := range []bool{false, true} {
		name := "the sound alone"
		if speech {
			name = "over speech that started under it"
		}
		t.Run(name, func(t *testing.T) {
			stream := &recordingStream{}
			out := audio.NewOutput(audio.OutputConfig{Open: func(string) (audio.PlaybackStream, int, error) {
				return stream, rate, nil
			}})
			t.Cleanup(func() { _ = out.Close() })
			light := &lightAt{stream: stream}
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
			if speech {
				pcm := make([]byte, 2*rate) // 1 s
				for i := 0; i < len(pcm); i += 2 {
					binary.LittleEndian.PutUint16(pcm[i:], speechLevel)
				}
				pb := out.Begin(audio.InputVoice)
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

			words := []voice.Word{{Text: "Wispr,", Start: 1, End: 1.15}, {Text: "never", Start: 1.2, End: 1.35}, {Text: "mind.", Start: 1.4, End: 1.55}}
			s.OnTranscript(&voice.Transcript{Text: "Wispr, never mind.", Words: words, IsFinal: true, SpeechFinal: true, GateOpen: true, At: time.Now()})
			var dropped int
			deadline := time.Now().Add(3 * time.Second)
			for {
				var ok bool
				if dropped, ok = light.wentOff(); ok {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("the never mind did not drop the held answer")
				}
				time.Sleep(time.Millisecond)
			}
			if speech {
				if err := <-spoken; err != nil {
					t.Fatalf("the speech under the sound = %v, want it played whole", err)
				}
			} else {
				time.Sleep(300 * time.Millisecond)
			}

			got := stream.played()
			if sound := countNonZero(got[:dropped]); sound >= 660*rate/1000 {
				t.Fatalf("%d samples of the answer sound played before the drop, want the drop to land within its 660 ms", sound)
			}
			if speech {
				// Control: the speech played ducked, with the sound over it,
				// before the drop; a speech at its own level means the sound
				// had already stopped and nothing here tests the ducking.
				under := 0
				for _, v := range got[speechFrom:dropped] {
					if v != 0 && v != speechLevel {
						under++
					}
				}
				if under < rate/50 {
					t.Fatalf("only %d samples of the speech played under the sound before the drop, want 20 ms or more", under)
				}
			}
			after := got[min(len(got), dropped+slack):]
			if !speech {
				if n := countNonZero(after); n > 0 {
					t.Fatalf("%d samples of the answer sound played more than two frames after the never mind dropped its answer", n)
				}
				return
			}
			// After the cut, only the speech: rising from ducked to its level
			// without a dip, then at its level until it ends.
			full := -1
			for i, v := range after {
				if v == 0 && full >= 0 {
					after = after[:i] // the speech ended
					break
				}
				if v < 0 || v > speechLevel || (i > 0 && v < after[i-1]) {
					t.Fatalf("sample %d after the cut is %d (the one before %d): more than the speech, which is %d at most and only rises back from ducked; the answer sound still plays", i, v, after[max(0, i-1)], speechLevel)
				}
				if v == speechLevel && full < 0 {
					full = i
				}
			}
			if full < 0 || full > rate/100 {
				t.Fatalf("the speech came back to %d at sample %d after the cut, want within 240 (10 ms)", speechLevel, full)
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
