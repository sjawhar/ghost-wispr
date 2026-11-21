package voice

import (
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

func TestReviewRealLongSpeechDelayedReports(t *testing.T) {
	d := &reviewPacedDevice{}
	out := audio.NewOutput(audio.OutputConfig{Open: func(string) (audio.PlaybackStream, int, error) { return d, 48000, nil }})
	t.Cleanup(func() { _ = out.Close() })
	var q *reviewOutputReports
	c := newFakeConcierge()
	r := newRigWith(t, c, func(cfg *SupervisorConfig, r *rig) {
		q = &reviewOutputReports{Speaker: SpeakerOf(out), fake: r.speaker, holding: true, seen: make(chan audio.SpeechEvent, 16)}
		cfg.Speaker = q
		cfg.Voice = func() (Voice, error) { return drivingVoice{r.voice}, nil }
		cfg.IdleWait, cfg.FollowUpWait = 2*time.Minute, 2*time.Minute
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
	began := time.Now()
	for range 1560 { // 31.2 s of real, paced 24 kHz PCM
		if err := pb.Write(t.Context(), pcm, audio.AudioFormat{SampleRate: 24000, Channels: 1, Encoding: audio.EncodingPCM16}); err != nil {
			t.Fatal(err)
		}
	}
	if err := pb.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	start, end := q.next(t), q.next(t)
	if !start.Started || end.Started || end.Cancelled || end.Played < 31*time.Second || end.At.Sub(start.At) < 30*time.Second {
		t.Fatalf("control: real long speech reports start=%+v end=%+v", start, end)
	}
	t.Logf("actual Output played=%v event span=%v wall time=%v", end.Played, end.At.Sub(start.At), time.Since(began))
	r.s.Stop("completed long speech still has both reports queued")
	eventually(t, "first exchange closed", func() bool { _, _, closed := first.state(); return closed })
	waitedFrom := time.Now()
	time.Sleep(ownWordsKept + 50*time.Millisecond)
	r.voice.failSays(errors.New("review: silent successor refuses Say"))
	c.answers <- PendingAnswer{TaskID: "long-later", SessionID: "sess-1", Text: "It ships tomorrow.", AskedAt: time.Now()}
	second := r.waitSession()
	eventually(t, "silent successor closed", func() bool { _, _, closed := second.state(); return closed })
	q.release()
	r.s.mu.Lock()
	reported := firstEx.notified[EventSpoken]
	r.s.mu.Unlock()
	t.Logf("reports held %v after the long exchange closed; first exchange spoken=%v", time.Since(waitedFrom), reported)
	if !reported {
		t.Fatal("completed long speech lost after a silent successor ended")
	}
	r.waitEvent("voice_spoken:sess-1")
}
