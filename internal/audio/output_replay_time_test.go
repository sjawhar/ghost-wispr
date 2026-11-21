package audio

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/monoclock"
)

// lockAfterSoundStream reports, with discard, that the device discarded its
// first frame, as a suspend recovery does, then takes the replay. Another
// goroutine holds the Output's lock for 150 ms from each write with sound,
// as a Write or Cancel can before the mixer records the frame. soundAt is
// when the last write with sound returned.
type lockAfterSoundStream struct {
	out     *Output
	discard bool
	writes  int // only the mixer touches it
	held    sync.WaitGroup

	mu      sync.Mutex
	soundAt time.Time
}

func (s *lockAfterSoundStream) Write(frame []int16) error {
	s.writes++
	if probeNonzero(frame) == 0 {
		return nil
	}
	s.out.mu.Lock()
	s.held.Add(1)
	go func() {
		defer s.held.Done()
		time.Sleep(150 * time.Millisecond)
		s.out.mu.Unlock()
	}()
	s.mu.Lock()
	s.soundAt = time.Now()
	s.mu.Unlock()
	return nil
}

func (s *lockAfterSoundStream) takeAgain() int {
	if s.discard && s.writes == 1 {
		return 480
	}
	return 0
}

func (*lockAfterSoundStream) Drop() (int, error) { return 0, nil }
func (s *lockAfterSoundStream) Close() error     { s.held.Wait(); return nil }

// TestAReplayedFrameIsTakenWhenItsWriteReturned fails if a speech whose first
// frame the device discarded, and the mixer wrote again, starts and ends when
// the replay's caller took the Output's lock, not when the replay's write
// returned: another goroutine held the lock for 150 ms, and the speech's
// times came 150 ms late, where main's are those of the write (Sol's review
// of #148 at 6ec0b3c). It fails too if an event's AtMono is not
// CLOCK_MONOTONIC read with its At, as when the mixer keeps its reading from
// before the replay. The control's device discards nothing.
func TestAReplayedFrameIsTakenWhenItsWriteReturned(t *testing.T) {
	for _, discard := range []bool{false, true} {
		t.Run(fmt.Sprintf("the first frame discarded %v", discard), func(t *testing.T) {
			stream := &lockAfterSoundStream{discard: discard}
			out, start, events := probeOutput(t, stream)
			stream.out = out
			if err := out.Begin(InputVoice, nil).Write(testContext(t), constant(48000, 10*time.Millisecond, 1000), mono48k); err != nil {
				t.Fatal(err)
			}
			base := monoclock.Now()
			start()
			began := probeVoiceEvent(t, events, true)
			ended := probeVoiceEvent(t, events, false)
			stream.mu.Lock()
			sound := stream.soundAt
			stream.mu.Unlock()
			if sound.IsZero() || ended.Cancelled {
				t.Fatalf("fixture: the last write with sound at %v, the end %+v", sound, ended)
			}
			for _, e := range []SpeechEvent{began, ended} {
				if lag := e.At.Sub(sound); lag < 0 || lag >= 100*time.Millisecond {
					t.Errorf("the speech's %s came %v after its write returned, want that write's time (started %v)", map[bool]string{true: "start", false: "end"}[e.Started], lag, e.Started)
				}
				wall, mono := e.At.Sub(base.Wall), time.Duration(e.AtMono-base.Mono)
				if d := wall - mono; d > time.Millisecond || d < -time.Millisecond {
					t.Errorf("the speech's %s came %v after the test's reading on the wall clock, %v on CLOCK_MONOTONIC: its AtMono was not read with its At", map[bool]string{true: "start", false: "end"}[e.Started], wall, mono)
				}
			}
		})
	}
}
