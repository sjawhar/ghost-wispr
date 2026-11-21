package voice

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

// Sol's probes from the reviews of #143, rounds 4 to 7, kept as tests: the
// real audio.Output's speech reports, held before they reach the Supervisor.

// reviewPacedDevice takes each frame as fast as a 48 kHz device plays it.
type reviewPacedDevice struct{}

func (*reviewPacedDevice) Write(frame []int16) error {
	time.Sleep(time.Duration(len(frame)) * time.Second / 48000)
	return nil
}
func (*reviewPacedDevice) Drop() (int, error) { return 0, nil }
func (*reviewPacedDevice) Close() error       { return nil }

// Delay delivery, preserving real Output reports and their device timestamps.
// The holder-answer path never calls FlushSpeech, including after #133.
type reviewOutputReports struct {
	Speaker
	fake    *fakeSpeaker
	mu      sync.Mutex
	holding bool
	queue   []heldReport
	seen    chan audio.SpeechEvent
}

func (q *reviewOutputReports) Begin(input string, owner any) Playback {
	if input == audio.InputChime {
		return q.fake.Begin(input, owner)
	}
	return q.Speaker.Begin(input, owner)
}
func (q *reviewOutputReports) Cancel(input string) {
	if input == audio.InputChime {
		q.fake.Cancel(input)
		return
	}
	q.Speaker.Cancel(input)
}
func (q *reviewOutputReports) OnSpeech(fn func(audio.SpeechEvent)) func() {
	return q.Speaker.OnSpeech(func(e audio.SpeechEvent) {
		q.mu.Lock()
		if q.holding {
			q.queue = append(q.queue, heldReport{fn: fn, e: e})
			q.mu.Unlock()
			q.seen <- e
			return
		}
		q.mu.Unlock()
		fn(e)
	})
}
func (q *reviewOutputReports) release() {
	q.mu.Lock()
	for len(q.queue) > 0 {
		item := q.queue[0]
		q.queue = q.queue[1:]
		q.mu.Unlock()
		item.fn(item.e)
		q.mu.Lock()
	}
	q.holding = false
	q.mu.Unlock()
}
func (q *reviewOutputReports) next(t *testing.T) audio.SpeechEvent {
	t.Helper()
	select {
	case e := <-q.seen:
		return e
	case <-time.After(testWait):
		t.Fatal("no real Output speech event")
		return audio.SpeechEvent{}
	}
}

func TestReviewDeviceStampedStartAfterLaterExchangeEnds(t *testing.T) {
	for _, laterCount := range []int{0, 1, 2} {
		laterEnd := laterCount > 0
		t.Run(fmt.Sprintf("ended_successors=%d", laterCount), func(t *testing.T) {
			d := &reviewPacedDevice{}
			out := audio.NewOutput(audio.OutputConfig{Open: func(string) (audio.PlaybackStream, int, error) { return d, 48000, nil }})
			t.Cleanup(func() { _ = out.Close() })
			var q *reviewOutputReports
			c := newFakeConcierge()
			r := newRigWith(t, c, func(cfg *SupervisorConfig, r *rig) {
				q = &reviewOutputReports{Speaker: SpeakerOf(out), fake: r.speaker, holding: true, seen: make(chan audio.SpeechEvent, 16)}
				cfg.Speaker = q
				cfg.Voice = func() (Voice, error) { return drivingVoice{r.voice}, nil }
				cfg.IdleWait, cfg.FollowUpWait = time.Minute, time.Minute
				cfg.SilenceWait, cfg.SoundGap = time.Millisecond, time.Millisecond
			})
			t.Cleanup(q.release)
			r.heard("Wispr, what is happening?", true, true)
			first := r.waitSession()
			r.waitEvent("voice_exchange_started:sess-1")
			r.s.mu.Lock()
			firstEx := r.s.ex
			r.s.mu.Unlock()
			pb := first.x.Out.Begin()
			pcm := make([]byte, 960)
			for i := 0; i < len(pcm); i += 2 {
				pcm[i] = 64
			}
			if err := pb.Write(t.Context(), pcm, audio.AudioFormat{SampleRate: 24000, Channels: 1, Encoding: audio.EncodingPCM16}); err != nil {
				t.Fatal(err)
			}
			start, end := q.next(t), q.next(t)
			if !start.Started || end.Started || end.Cancelled {
				t.Fatalf("control: actual Output events %+v %+v", start, end)
			}
			// Sol's round-5 probe also checked that the start's At follows the
			// device's first write: #133's change of At, not this PR's. The
			// owner, not At, makes the speech the first exchange's.
			if start.Owner != firstEx || end.Owner != firstEx {
				t.Fatalf("the real Output's reports carry %p and %p, want the first exchange's playback's owner %p", start.Owner, end.Owner, firstEx)
			}
			t.Logf("real Output speech played %v", end.Played)
			r.s.Stop("completed first exchange ends with both real Output reports queued")
			eventually(t, "first closed", func() bool { _, _, closed := first.state(); return closed })
			if !laterEnd {
				q.release()
			}
			// No transcript and no call of FlushSpeech creates the successor.
			// A pending answer arriving after Stop may take the free floor.
			r.voice.failSays(errors.New("review: successor's vendor refused Say"))
			for i := range max(1, laterCount) {
				c.answers <- PendingAnswer{TaskID: fmt.Sprintf("later-%d", i), SessionID: "sess-1", Text: "It ships tomorrow.", AskedAt: time.Now()}
				second := r.waitSession()
				eventually(t, "silent holder-answer exchange closed", func() bool { _, _, closed := second.state(); return closed })
			}
			q.release()
			r.s.mu.Lock()
			reported := firstEx.notified[EventSpoken]
			r.s.mu.Unlock()
			if !reported {
				t.Fatal("completed device-stamped first speech lost after the later holder-answer exchange ended")
			}
			r.waitEvent("voice_spoken:sess-1")
		})
	}
}
