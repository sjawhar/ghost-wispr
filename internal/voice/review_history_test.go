package voice

import (
	"errors"
	"runtime"
	"testing"
	"time"
	"weak"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

// This uses real Output, not a Speaker that invents or drops callbacks.
// The device fails to open after a Write has queued its frame. Such a
// playback never starts a speech, so neither a start nor an end is emitted.
// From review 6007067736 of #143, which found eight such exchanges kept in
// the Supervisor's floor history. Attribution is by the playback's owner now
// (audio.SpeechEvent.Owner), and the Supervisor keeps no history: the
// failed exchanges must report no speech, none is spoken, and every one of
// them must be collectable once it ended (review 6007806985 asked that the
// retention check stay a retention check).
func TestReviewDeviceUnavailableHistory(t *testing.T) {
	for _, queued := range []bool{false, true} {
		name := "control_no_audio_queued"
		if queued {
			name = "device_open_fails_after_audio_queued"
		}
		t.Run(name, func(t *testing.T) {
			out := audio.NewOutput(audio.OutputConfig{Open: func(string) (audio.PlaybackStream, int, error) {
				return nil, 0, errors.New("review: speaker device unavailable")
			}})
			t.Cleanup(func() { _ = out.Close() })
			var q *reviewOutputReports
			r := newRigWith(t, newFakeConcierge(), func(cfg *SupervisorConfig, r *rig) {
				q = &reviewOutputReports{Speaker: SpeakerOf(out), fake: r.speaker, holding: true, seen: make(chan audio.SpeechEvent, 16)}
				cfg.Speaker = q
				cfg.Voice = func() (Voice, error) { return drivingVoice{r.voice}, nil }
				cfg.IdleWait, cfg.FollowUpWait = time.Minute, time.Minute
			})
			t.Cleanup(q.release)
			const exchanges = 8
			var ended []weak.Pointer[exchange]
			for range exchanges {
				r.heard("Wispr, what is happening?", true, true)
				sess := r.waitSession()
				r.waitEvent("voice_exchange_started:sess-1")
				r.s.mu.Lock()
				ended = append(ended, weak.Make(r.s.ex))
				r.s.mu.Unlock()
				if queued {
					// Once an earlier open failed, the Output may refuse the
					// write itself: nothing is queued then either.
					pb := sess.x.Out.Begin()
					if err := pb.Write(t.Context(), make([]byte, 960), audio.AudioFormat{SampleRate: 24000, Channels: 1, Encoding: audio.EncodingPCM16}); err == nil {
						if err := pb.Drain(t.Context()); err == nil {
							t.Fatal("control: missing device did not fail Drain")
						}
					}
				}
				r.s.Stop("review: exchange ended after unavailable speaker")
				eventually(t, "the exchange closed", func() bool { _, _, closed := sess.state(); return closed })
			}
			r.heard("Wispr, what happens next?", true, true)
			silent := r.waitSession()
			r.waitEvent("voice_exchange_started:sess-1")
			r.s.Stop("review: silent successor ends")
			eventually(t, "silent successor closed", func() bool { _, _, closed := silent.state(); return closed })
			q.mu.Lock()
			reports := len(q.queue)
			q.mu.Unlock()
			t.Logf("ended=%d actual_Output_reports=%d", exchanges, reports)
			if reports != 0 {
				t.Fatal("control: an unavailable device unexpectedly reported speech")
			}
			q.release()
			if n := spokenCount(r); n != 0 {
				t.Errorf("voice_spoken %d times for exchanges whose speech never played", n)
			}
			// The test's fakes keep every exchange they saw: the fake
			// concierge each Exchange it heard or answered, whose Out holds
			// its exchange, and the fake voice each session. While they do,
			// every exchange must be alive: the positive control that the
			// weak pointers see a live exchange. Once they let go, what
			// remains is only what the Supervisor and the Output keep, and
			// that must be nothing: every exchange is collected within two
			// seconds, the time a goroutine still finishing an exchange's
			// close, on a loaded machine, can hold it.
			alive := func() int {
				runtime.GC()
				n := 0
				for _, w := range ended {
					if w.Value() != nil {
						n++
					}
				}
				return n
			}
			if n := alive(); n != exchanges {
				t.Fatalf("control: %d of %d exchanges alive while the test's fakes still hold them, want all", n, exchanges)
			}
			r.voice.mu.Lock()
			r.voice.sessions = nil
			r.voice.mu.Unlock()
			r.concierge.mu.Lock()
			r.concierge.heard, r.concierge.exchanges = nil, nil
			r.concierge.mu.Unlock()
			n := alive()
			for deadline := time.Now().Add(2 * time.Second); n != 0 && time.Now().Before(deadline); n = alive() {
				time.Sleep(10 * time.Millisecond)
			}
			runtime.KeepAlive(out)
			runtime.KeepAlive(r)
			if n != 0 {
				t.Errorf("%d of %d ended exchanges still reachable through the Supervisor or the Output two seconds after the unavailable speaker refused their audio", n, exchanges)
			}
		})
	}
}
