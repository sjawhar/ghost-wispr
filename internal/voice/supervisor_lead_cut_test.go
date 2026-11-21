package voice

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

// TestALeadCutBeforeTheFactIsReadyLeadsTheAnswer fails if R5's line naming
// the issue, whose Say succeeded, does not lead the answer when a cut came
// after the line's call and before the voice was quiet once the answer's fact
// was ready, or leads it when no cut came. A cut of a later speech of the
// same exchange counts too: the room then hears the issue's name twice, the
// limit this rule accepts, where judging the line by its own silence took a
// speaker state each review round found another hole in (rounds 10 to 12 of
// #133).
func TestALeadCutBeforeTheFactIsReadyLeadsTheAnswer(t *testing.T) {
	for _, tc := range []struct {
		name string
		// cut: the Supervisor cuts the voice before the fact is ready, as a
		// barge-in does; later: a later speech starts after the line ends,
		// and the cut is its.
		cut, later bool
		want       string
	}{
		{"control: no cut", false, false, "The issue is in review."},
		{"the line itself cut", true, false, "I took ABC-12. The issue is in review."},
		{"a later speech cut, the accepted limit", true, true, "I took ABC-12. The issue is in review."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newFakeConcierge("The issue is in review.")
			c.acks, c.ackMode = []string{"I took ABC-12."}, Lead
			c.afterAcks = make(chan struct{})
			r := newRigWith(t, c, func(cfg *SupervisorConfig, r *rig) {
				cfg.Voice = func() (Voice, error) { return drivingVoice{r.voice}, nil }
				cfg.IdleWait, cfg.FollowUpWait = time.Minute, time.Minute
			})
			r.s.OnTranscript(timedResult("Wispr, what's the status of ABC-12?", 10, true, from(time.Now().Add(-3*time.Second))))
			sess := r.waitSession()
			out := make(chan json.RawMessage, 1)
			go func() {
				res, _ := sess.x.Tools.Run(t.Context(), ToolCall{ID: "d1", Name: AnswerTool, Args: json.RawMessage(`{"question":"q"}`)})
				out <- res
			}()
			if got := waitSaid(t, sess); got != "I took ABC-12." {
				t.Fatalf("said %q at once, want R5's line", got)
			}
			first := waitSpeechAfter(t, r, time.Time{})
			if tc.later {
				r.speaker.finish(audio.InputVoice) // the line plays to its end
				r.speaker.FlushSpeech()
				speakAs(t, sess, "Let me check one more thing.")
				waitSpeechAfter(t, r, first)
			}
			if tc.cut {
				cutVoice(t, r)
				r.speaker.FlushSpeech()
			} else {
				r.speaker.finish(audio.InputVoice)
			}
			close(c.afterAcks)
			select {
			case res := <-out:
				var got AnswerResult
				if err := json.Unmarshal(res, &got); err != nil || got.Say != tc.want {
					t.Fatalf("the answer tool returned %s, want %q", res, tc.want)
				}
			case <-time.After(4 * time.Second):
				t.Fatal("the answer tool did not return")
			}
		})
	}
}

// drainingLeadVoice opens the rig's fake sessions, as a voice that drives its
// own turn, whose Say queues 20 ms of audio on the exchange's output and
// returns once it is queued, then drains it in the background, as Deepgram's
// Voice Agent does: its Say never learns what became of the audio.
type drainingLeadVoice struct {
	r       *rig
	drained chan error
}

func (drainingLeadVoice) DrivesTurn() bool { return true }

func (v drainingLeadVoice) Open(ctx context.Context, x *Exchange) (Session, error) {
	base, err := v.r.voice.Open(ctx, x)
	if err != nil {
		return nil, err
	}
	return drainingLeadSession{Session: base, out: x.Out, drained: v.drained}, nil
}

type drainingLeadSession struct {
	Session
	out     Output
	drained chan error
}

func (s drainingLeadSession) Say(ctx context.Context, _ string, _ SayMode) error {
	pb := s.out.Begin()
	if err := pb.Write(ctx, voicedPCM(20*time.Millisecond), mono48kFormat); err != nil {
		return err
	}
	go func() { s.drained <- pb.Drain(context.Background()) }()
	return nil
}

// openTestExchange opens an exchange on the rig as a call opens it, with
// the rig's voice, but without the wake chime: no other playback reaches the
// speaker.
func openTestExchange(t *testing.T, r *rig, id string) (*exchange, exchangeTools) {
	t.Helper()
	ctx := newExchangeContext(r.s.ctx)
	ex := &exchange{ctx: ctx, cancel: ctx.cancel, authority: ctx, opened: make(chan struct{}), drives: true, wakeStart: -1, notified: map[string]bool{}, times: &exchangeTimes{}}
	ex.x = Exchange{SessionID: id, WakeAt: time.Now(), Out: exchangeOutput{s: r.s, ex: ex}}
	tools := exchangeTools{s: r.s, ex: ex}
	ex.x.Tools = tools
	r.s.mu.Lock()
	r.s.takeFloorLocked(ex)
	r.s.mu.Unlock()
	v, err := r.s.cfg.Voice()
	if err != nil {
		t.Fatal(err)
	}
	r.s.open(ex, v, &ex.x, false)
	return ex, tools
}

// heldSecondWriteStream is a speaker stream whose second write with audio in
// it waits until release closes, having closed entered; else it plays in
// real time.
type heldSecondWriteStream struct {
	voiced           int // only the mixer touches it
	entered, release chan struct{}
}

func (s *heldSecondWriteStream) Write(frame []int16) error {
	if slices.ContainsFunc(frame, func(v int16) bool { return v != 0 }) {
		if s.voiced++; s.voiced == 2 {
			close(s.entered)
			<-s.release
		}
	}
	time.Sleep(time.Duration(len(frame)) * time.Second / 48000)
	return nil
}

func (*heldSecondWriteStream) Drop() (int, error) { return 0, nil }
func (*heldSecondWriteStream) Close() error       { return nil }

// TestAnEarlierCutIsNotANewLeadsCut fails if R5's line naming the issue,
// called after the Supervisor cut an earlier speech and played with no cut,
// is not heard because the speaker reported the earlier speech's cancelled
// end only once its mixer took the Cancel in, after the line's call: that
// end was taken for a second cut (Sol's review of #133 at f398ca8). It runs
// the real speaker output, whose second write of the earlier speech is held
// across the cut. The control's earlier end is delivered before the call.
func TestAnEarlierCutIsNotANewLeadsCut(t *testing.T) {
	for _, late := range []bool{false, true} {
		t.Run(fmt.Sprintf("the earlier end comes after the call %v", late), func(t *testing.T) {
			stream := &heldSecondWriteStream{entered: make(chan struct{}), release: make(chan struct{})}
			out := audio.NewOutput(audio.OutputConfig{Open: func(string) (audio.PlaybackStream, int, error) {
				return stream, 48000, nil
			}})
			t.Cleanup(func() {
				if err := out.Close(); err != nil {
					t.Errorf("close the speaker output: %v", err)
				}
			})
			release := sync.OnceFunc(func() { close(stream.release) })
			t.Cleanup(release)
			drained := make(chan error, 1)
			r := newRigWith(t, newFakeConcierge(), func(cfg *SupervisorConfig, r *rig) {
				cfg.Speaker = SpeakerOf(out)
				cfg.Voice = func() (Voice, error) { return drainingLeadVoice{r: r, drained: drained}, nil }
				cfg.IdleWait, cfg.FollowUpWait = time.Minute, time.Minute
			})
			ex, tools := openTestExchange(t, r, "sess-earlier-cut")
			earlier := ex.x.Out.Begin()
			for range 5 { // 100 ms of an earlier speech, in frames the speaker takes
				if err := earlier.Write(t.Context(), voicedPCM(20*time.Millisecond), mono48kFormat); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-stream.entered:
			case <-time.After(testWait):
				t.Fatal("control: the earlier speech's second write was not held")
			}
			cutVoice(t, r)
			r.s.mu.Lock()
			cutAt := ex.cutAt
			r.s.mu.Unlock()
			if !late {
				release()
				eventually(t, "the earlier speech's end", func() bool {
					out.FlushSpeech()
					r.s.mu.Lock()
					defer r.s.mu.Unlock()
					return !r.s.voiceSpeaking
				})
			}
			line := tools.speakLead("I took ABC-12.", Lead)
			select {
			case <-line.done:
			case <-time.After(testWait):
				t.Fatal("the line's Say did not return")
			}
			if !line.ok || !line.called.After(cutAt) {
				t.Fatal("control: the line must succeed, called after the only cut")
			}
			release()
			select {
			case err := <-drained:
				if err != nil {
					t.Fatalf("control: the line's Drain = %v", err)
				}
			case <-time.After(testWait):
				t.Fatal("the line's Drain did not return")
			}
			if !tools.heard(line) {
				r.s.mu.Lock()
				recorded := ex.cutAt
				r.s.mu.Unlock()
				t.Fatalf("the uncut line was not heard: the only cut came %v before its call, and a cut was recorded %v after it", line.called.Sub(cutAt), recorded.Sub(line.called))
			}
		})
	}
}

// cutVoice cuts the exchange's voice as a barge-in or a stop does: with the
// Supervisor's own Cancel, which records the cut (cutDriverLocked).
func cutVoice(t *testing.T, r *rig) {
	t.Helper()
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	if r.s.ex == nil {
		t.Fatal("no exchange holds the floor to cut")
	}
	r.s.cutDriverLocked(r.s.ex)
}

// mono48kFormat is 48 kHz mono PCM16.
var mono48kFormat = audio.AudioFormat{SampleRate: 48000, Channels: 1, Encoding: audio.EncodingPCM16}

// voicedPCM is d of a constant tone, 48 kHz mono PCM16.
func voicedPCM(d time.Duration) []byte {
	pcm := make([]byte, 2*int(48000*d/time.Second))
	for i := 0; i < len(pcm); i += 2 {
		binary.LittleEndian.PutUint16(pcm[i:], 1000)
	}
	return pcm
}
