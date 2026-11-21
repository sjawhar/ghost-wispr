package voice

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

type reviewLosingDevice struct {
	fail      atomic.Bool
	lost      chan struct{}
	once      sync.Once
	closed    chan struct{}
	closeOnce sync.Once
}

func (d *reviewLosingDevice) Write(frame []int16) error {
	time.Sleep(time.Duration(len(frame)) * time.Second / 48000)
	if d.fail.Load() {
		d.once.Do(func() { close(d.lost) })
		return errors.New("review: speaker lost after completed speech")
	}
	return nil
}
func (*reviewLosingDevice) Drop() (int, error) { return 0, nil }
func (d *reviewLosingDevice) Close() error     { d.closeOnce.Do(func() { close(d.closed) }); return nil }

// Real Output supplies both reports. Only their delivery to Supervisor is
// delayed. The device completes the speech before any drain or injected loss.
func TestReviewDrainOwnership(t *testing.T) {
	for _, tc := range []struct{ name, drain, delivery string }{
		{"successful_drain_both_reports_held", "success", "held"},
		{"failed_drain_reports_delivered_early", "fail", "early"},
		{"failed_drain_start_delivered_early", "fail", "start"},
		{"failed_drain_both_reports_held", "fail", "held"},
		{"never_drain_reports_delivered_early", "none", "early"},
		{"never_drain_both_reports_held", "none", "held"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &reviewLosingDevice{lost: make(chan struct{}), closed: make(chan struct{})}
			out := audio.NewOutput(audio.OutputConfig{Open: func(string) (audio.PlaybackStream, int, error) { return d, 48000, nil }})
			t.Cleanup(func() { _ = out.Close() })
			var q *reviewOutputReports
			r := newRigWith(t, newFakeConcierge(), func(cfg *SupervisorConfig, r *rig) {
				q = &reviewOutputReports{Speaker: SpeakerOf(out), fake: r.speaker, holding: true, seen: make(chan audio.SpeechEvent, 16)}
				cfg.Speaker = q
				cfg.Voice = func() (Voice, error) { return drivingVoice{r.voice}, nil }
				cfg.IdleWait, cfg.FollowUpWait = time.Minute, time.Minute
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
			if !start.Started || end.Started || end.Cancelled || end.Played < 20*time.Millisecond {
				t.Fatalf("control: not a completed real speech: start=%+v end=%+v", start, end)
			}
			switch tc.drain {
			case "success":
				if err := pb.Drain(t.Context()); err != nil {
					t.Fatal(err)
				}
			case "fail":
				d.fail.Store(true)
				waitFor(t, d.lost, "the real Output's stream failing")
				waitFor(t, d.closed, "the real Output finishing failure handling")
				if err := pb.Drain(t.Context()); err == nil {
					t.Fatal("control: device loss did not fail Drain")
				} else {
					t.Logf("Drain returned %v", err)
				}
			}
			if tc.delivery == "early" {
				q.release()
			}
			if tc.delivery == "start" {
				q.mu.Lock()
				item := q.queue[0]
				q.queue = q.queue[1:]
				q.mu.Unlock()
				item.fn(item.e)
			}
			r.s.Stop("review: completed speech before delayed report")
			eventually(t, "first exchange closed", func() bool { _, _, closed := first.state(); return closed })
			// The reports' times move 45 s back, past the 30 s the round-6 rule
			// kept a span for: attribution reads no time.
			q.mu.Lock()
			for i := range q.queue {
				q.queue[i].e.At = q.queue[i].e.At.Add(-45 * time.Second)
			}
			q.mu.Unlock()
			r.heard("Wispr, what happens next?", true, true)
			silent := r.waitSession()
			r.waitEvent("voice_exchange_started:sess-1")
			r.s.mu.Lock()
			silentEx := r.s.ex
			r.s.mu.Unlock()
			r.s.Stop("review: silent successor invokes floor pruning")
			eventually(t, "silent successor closed", func() bool { _, _, closed := silent.state(); return closed })
			q.release()
			r.s.mu.Lock()
			firstSpoken, laterSpoken := firstEx.notified[EventSpoken], silentEx.notified[EventSpoken]
			r.s.mu.Unlock()
			t.Logf("real_Output_played=%v cancelled=%v first_voice_spoken=%v silent_voice_spoken=%v", end.Played, end.Cancelled, firstSpoken, laterSpoken)
			if !firstSpoken || laterSpoken {
				t.Errorf("completed speech lost or misowned: first=%v silent=%v; want true,false", firstSpoken, laterSpoken)
			}
			if firstSpoken {
				eventually(t, "exactly one spoken event", func() bool { return spokenCount(r) == 1 })
			}
		})
	}
}
