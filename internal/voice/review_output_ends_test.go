package voice

import (
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

// reviewStoppedDevice takes writes until one with audio in it has returned,
// so the speech has started (#133 starts a speech only once the stream took
// its first audio), and then holds the next write until release closes, and
// fails it if fail is set.
type reviewStoppedDevice struct {
	mu      sync.Mutex
	took    bool // a write with audio in it returned
	once    sync.Once
	entered chan struct{}
	release chan struct{}
	fail    bool
}

func (d *reviewStoppedDevice) Write(frame []int16) error {
	d.mu.Lock()
	took := d.took
	if !took && slices.ContainsFunc(frame, func(v int16) bool { return v != 0 }) {
		d.took = true
	}
	d.mu.Unlock()
	if !took {
		return nil
	}
	d.once.Do(func() { close(d.entered) })
	<-d.release
	if d.fail {
		return errors.New("review: speaker device lost")
	}
	return nil
}
func (*reviewStoppedDevice) Drop() (int, error) { return 0, nil }
func (*reviewStoppedDevice) Close() error       { return nil }

// TestReviewStartedOutputAlwaysEnds is Sol's round-6 probe of #143: a speech
// the real Output started must report its end, whether the Output closes
// while a later device write is blocked, or that write reports the device
// lost. Adapted to #133, which reports a start only once the stream has
// taken the speech's first audio: the device takes the write with audio,
// and blocks or fails the next one, so the speech has started first.
func TestReviewStartedOutputAlwaysEnds(t *testing.T) {
	for _, lost := range []bool{false, true} {
		name := "close_with_device_write_blocked"
		if lost {
			name = "device_write_reports_lost"
		}
		t.Run(name, func(t *testing.T) {
			d := &reviewStoppedDevice{entered: make(chan struct{}), release: make(chan struct{}), fail: lost}
			unblock := sync.OnceFunc(func() { close(d.release) })
			out := audio.NewOutput(audio.OutputConfig{Open: func(string) (audio.PlaybackStream, int, error) { return d, 48000, nil }})
			t.Cleanup(func() { unblock(); _ = out.Close() })
			events := make(chan audio.SpeechEvent, 8)
			remove := out.OnSpeech(func(e audio.SpeechEvent) { events <- e })
			defer remove()
			next := func() audio.SpeechEvent {
				select {
				case e := <-events:
					return e
				case <-time.After(testWait):
					t.Fatal("Output did not report")
					return audio.SpeechEvent{}
				}
			}
			pb := out.Begin(audio.InputVoice, nil)
			pcm := make([]byte, 960)
			for i := 0; i < len(pcm); i += 2 {
				pcm[i] = 64 // audio, not silence: the device takes it
			}
			if err := pb.Write(t.Context(), pcm, audio.AudioFormat{SampleRate: 24000, Channels: 1, Encoding: audio.EncodingPCM16}); err != nil {
				t.Fatal(err)
			}
			waitFor(t, d.entered, "the device write after the speech's first audio entered")
			start := next()
			closed := make(chan error, 1)
			if lost {
				unblock()
			} else {
				go func() { closed <- out.Close() }()
			}
			end := next()
			if !start.Started || end.Started || !end.Cancelled || end.At.Before(start.At) {
				t.Fatalf("unordered or absent terminal reports: start=%+v end=%+v", start, end)
			}
			t.Logf("real Output report order: start then cancelled end; interval=%v", end.At.Sub(start.At))
			unblock()
			if !lost {
				select {
				case err := <-closed:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(testWait):
					t.Fatal("Close did not return")
				}
			}
		})
	}
}
