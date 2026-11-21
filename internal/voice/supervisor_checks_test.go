package voice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

// The tests here carry the names of slice 1's checks in verification.md
// (c-tap, c-wake, c-floor, c-stop, c-voices).

// --- c-tap ---

// TestTapPassesNothingWhileGateClosed fails if room audio written while the
// capture gate is closed, or before the closing, reaches an exchange after it.
func TestTapPassesNothingWhileGateClosed(t *testing.T) {
	r := newDrivingRig(t, time.Minute)
	r.tap.SetRate(16000)
	if _, err := r.tap.Write(pcmBytes(16000, 0)); err != nil { // 1 s before the closing
		t.Fatal(err)
	}
	r.closeGate()
	r.heard("Wispr, what is happening?", true, true)
	sess := r.waitSession()
	select {
	case f := <-sess.x.Tap:
		t.Fatalf("the exchange got %d samples of audio from before the closing", len(f.PCM))
	case <-time.After(100 * time.Millisecond):
	}
}

// TestSupervisorDropsQueuedTapFrames fails if frames still queued for a voice
// when a stop comes can be read after it.
func TestSupervisorDropsQueuedTapFrames(t *testing.T) {
	r := newDrivingRig(t, time.Minute)
	r.tap.SetRate(16000)
	r.heard("Wispr, what is happening?", true, true)
	sess := r.waitSession()
	for range 3 {
		if _, err := r.tap.Write(pcmBytes(4000, 0)); err != nil {
			t.Fatal(err)
		}
	}
	r.closeGate()
	for f := range sess.x.Tap {
		t.Fatalf("a queued frame of %d samples was read after the stop", len(f.PCM))
	}
}

// --- c-wake ---

// TestOwnSpeechWakesNothing fails if Wispr's own words, heard back while it
// speaks, open an exchange, even when they hold its name.
func TestOwnSpeechWakesNothing(t *testing.T) {
	c := newFakeConcierge("Wispr here: the voice issue is in review.")
	c.hold = true
	r := newRig(t, c)
	sess := r.ask("what is happening?")
	waitSaid(t, sess)
	r.waitSpeaking()
	r.heard("Wispr here: the voice issue is in review.", true, true)
	never(t, "Wispr's own words opened an exchange", 300*time.Millisecond, func() bool {
		_, _, closed := sess.state()
		return closed || len(r.voice.opened) > 0 || len(c.asked) > 0
	})
}

// --- c-floor ---

// TestOneVoiceAtATime fails if a held answer opens a voice while an exchange
// still holds the floor.
func TestOneVoiceAtATime(t *testing.T) {
	c := newFakeConcierge("The voice issue is in review.")
	r := newRig(t, c)
	first := r.ask("what is happening?")
	waitSaid(t, first)
	c.answers <- PendingAnswer{TaskID: "t1", SessionID: "sess-1", Text: "It ships tomorrow.", AskedAt: time.Now()}
	never(t, "a second voice opened while the exchange spoke", 300*time.Millisecond, func() bool { return len(r.voice.opened) > 0 })
	r.speaker.finish(audio.InputVoice)
	second := r.waitSession()
	_, closedAt, closed := first.state()
	if !closed || closedAt.IsZero() {
		t.Fatal("the held answer's voice opened before the exchange closed")
	}
	if got := waitSaid(t, second); got != "It ships tomorrow." {
		t.Fatalf("said %q", got)
	}
}

// TestPendingWaitsForFreeFloor fails if an answer whose question a room word
// followed speaks by itself, or one whose question no room word followed
// does not (R7: the floor stayed free since the question).
func TestPendingWaitsForFreeFloor(t *testing.T) {
	t.Run("free floor", func(t *testing.T) {
		c := newFakeConcierge()
		r := newRig(t, c)
		c.answers <- PendingAnswer{TaskID: "t1", SessionID: "sess-1", Text: "It ships tomorrow.", AskedAt: time.Now()}
		if got := waitSaid(t, r.waitSession()); got != "It ships tomorrow." {
			t.Fatalf("said %q", got)
		}
	})
	t.Run("a word since the question", func(t *testing.T) {
		c := newFakeConcierge()
		r := newRig(t, c)
		asked := time.Now()
		time.Sleep(5 * time.Millisecond)
		r.heard("we should also look at the budget", true, true)
		c.answers <- PendingAnswer{TaskID: "t1", SessionID: "sess-1", Text: "It ships tomorrow.", AskedAt: asked}
		r.waitEvent("voice_answer_ready:sess-1")
		never(t, "an answer spoke by itself after a room word followed its question", 700*time.Millisecond, func() bool { return len(r.voice.opened) > 0 })
		r.heard("Wispr, go ahead.", true, true)
		if got := waitSaid(t, r.waitSession()); got != "It ships tomorrow." {
			t.Fatalf("said %q", got)
		}
	})
}

// TestSoundsWaitForAGap fails if the answer sound plays while people talk,
// before a gap of SoundGap with no transcript word.
func TestSoundsWaitForAGap(t *testing.T) {
	c := newFakeConcierge()
	r := newRig(t, c)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(30 * time.Millisecond):
				r.heard("people keep talking", false, false)
			}
		}
	}()
	r.heard("people keep talking", false, false) // already talking as the answer lands
	c.answers <- PendingAnswer{TaskID: "t1", SessionID: "sess-1", Text: "It ships tomorrow."}
	r.waitEvent("voice_answer_ready:sess-1")
	never(t, "the answer sound played while people talked", 400*time.Millisecond, func() bool { return len(r.speaker.soundsPlayed()) > 0 })
	close(stop)
	wg.Wait()
	eventually(t, "the answer sound after the gap", func() bool { return slices.Equal(r.speaker.soundsPlayed(), []string{"answer"}) })
}

// TestSoundTriggersAndPitch fails if a sound does not play on its trigger, or
// shares both its note count and a starting pitch within 4 semitones with
// another sound (R10). TestChimesAreShortAndDistinct bounds their lengths.
func TestSoundTriggersAndPitch(t *testing.T) {
	triggers := map[string]func(r *rig){
		"wake":   func(r *rig) { r.heard("Wispr,", false, false) },
		"answer": func(r *rig) { r.concierge.answers <- PendingAnswer{TaskID: "t1", SessionID: "sess-1", Text: "x"} },
		"hand": func(r *rig) {
			_ = r.s.RaiseHand(&Hand{ID: "h1", SessionID: "sess-1", Agent: "a1", IssueKey: "ABC-12", Name: "ABC-12", Text: "x", Expires: time.Now().Add(time.Minute)})
		},
		"start":              func(r *rig) { r.s.Started() },
		"error, a red check": func(r *rig) { r.s.RedCheckFailed() },
		"error, no voice": func(r *rig) {
			r.mu.Lock()
			r.voiceErr = errors.New("no such engine")
			r.mu.Unlock()
			r.heard("Wispr,", false, false)
		},
		"error, vendor fails": func(r *rig) { r.voice.failOpens(errors.New("refused")); r.heard("Wispr,", false, false) },
		"error, unreachable": func(r *rig) {
			r.concierge.answerErr = fmt.Errorf("search: %w", ErrUnreachable)
			r.ask("what is happening?")
		},
	}
	for name, trigger := range triggers {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, newFakeConcierge())
			trigger(r)
			want := strings.SplitN(name, ",", 2)[0]
			eventually(t, "the "+want+" sound", func() bool { return slices.Contains(r.speaker.soundsPlayed(), want) })
		})
	}
	names := slices.Sorted(maps.Keys(chimeNotes))
	for i, a := range names {
		for _, b := range names[i+1:] {
			na, nb := chimeNotes[a], chimeNotes[b]
			semitones := math.Abs(12 * math.Log2(na[0].hz/nb[0].hz))
			if len(na) == len(nb) && semitones < 4-1e-9 {
				t.Errorf("the %s and %s sounds both have %d notes and start %.1f semitones apart, want 4 or more", a, b, len(na), semitones)
			}
		}
	}
}

// answerSoundStops fails t if the answer sound, the first sound on the chime
// input, plays another frame within 300 ms (15 of its paced frames) once
// settle has passed, or if it played to its end.
func answerSoundStops(t *testing.T, r *rig, settle time.Duration) {
	t.Helper()
	answerFrames := (len(AnswerChime()) + 959) / 960
	time.Sleep(settle)
	_, frames := r.speaker.chimePlaybacks()
	before := frames[0]
	time.Sleep(300 * time.Millisecond)
	_, frames = r.speaker.chimePlaybacks()
	if frames[0] != before || frames[0] >= answerFrames {
		t.Fatalf("the answer sound played on after the wake: %d frames, then %d, of its %d", before, frames[0], answerFrames)
	}
}

// TestAWakeCutsTheAnswerChime fails if the answer sound plays on over the
// wake sound of a new call: once the wake sound has played, the answer sound
// must play no more frames. The wake asks nothing, so the exchange ends after
// TurnWait, and its end stops its wake sound: TurnWait is far longer than the
// wake sound's 12 paced frames here, as DefaultTurnWait (4 s) is on the unit,
// so the end cannot cut the sound the test waits for. (The rig's 300 ms left
// 60 ms, which a loaded machine took: on main, 2 runs in 40 failed under
// -race, and 6 in 20 without it.)
func TestAWakeCutsTheAnswerChime(t *testing.T) {
	c := newFakeConcierge()
	r := newRigWith(t, c, func(cfg *SupervisorConfig, _ *rig) { cfg.TurnWait = DefaultTurnWait })
	r.speaker.paceWrites(audio.InputChime, 20*time.Millisecond)
	c.answers <- PendingAnswer{TaskID: "t1", SessionID: "sess-1", Text: "x"}
	eventually(t, "the answer sound begins", func() bool { return len(r.speaker.soundsPlayed()) == 1 })
	time.Sleep(60 * time.Millisecond)
	r.heard("Wispr,", false, false)
	r.waitSession()
	wakeFrames := (len(WakeChime()) + 959) / 960
	eventually(t, "the whole wake sound", func() bool {
		names, frames := r.speaker.chimePlaybacks()
		return len(names) == 2 && names[1] == "wake" && frames[1] == wakeFrames
	})
	answerSoundStops(t, r, 0)
}

// TestAWakeThatStartsNoExchangeStillCutsTheAnswerChime fails if the answer
// sound plays on after a wake whose exchange does not start: one whose voice
// cannot be built, which plays the error sound instead, and one heard as the
// light stops being red. From review 6004985046 of #144: the answer sound
// played all its frames over the error sound.
func TestAWakeThatStartsNoExchangeStillCutsTheAnswerChime(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fail  func(r *rig)
		sound string // the sound the wake plays instead, if any
	}{
		{"the voice cannot be built", func(r *rig) { r.voiceErr = errors.New("no voice configured") }, "error"},
		{"the light is not red", func(r *rig) { r.red = false }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newFakeConcierge()
			r := newRig(t, c)
			r.speaker.paceWrites(audio.InputChime, 20*time.Millisecond)
			c.answers <- PendingAnswer{TaskID: "t1", SessionID: "sess-1", Text: "x"}
			eventually(t, "the answer sound begins", func() bool { return len(r.speaker.soundsPlayed()) == 1 })
			time.Sleep(60 * time.Millisecond)
			r.mu.Lock()
			tc.fail(r)
			r.mu.Unlock()
			r.heard("Wispr,", false, false)
			if tc.sound != "" {
				eventually(t, "the "+tc.sound+" sound", func() bool {
					return slices.Contains(r.speaker.soundsPlayed(), tc.sound)
				})
			}
			answerSoundStops(t, r, 100*time.Millisecond)
			select {
			case <-r.voice.opened:
				t.Fatal("control: the wake opened a voice")
			default:
			}
		})
	}
}

// --- c-stop ---

// voiceUnderTest opens an exchange of one kind of voice and gets it speaking.
type voiceUnderTest struct {
	name  string
	start func(t *testing.T) (*rig, *fakeSession)
}

func everyVoice() []voiceUnderTest {
	driving := func(t *testing.T) (*rig, *fakeSession) {
		r := newDrivingRig(t, time.Minute)
		r.heard("Wispr, what is happening?", true, true)
		sess := r.waitSession()
		speakAs(t, sess, "The voice issue is in review since this morning.")
		r.waitSpeaking()
		return r, sess
	}
	return []voiceUnderTest{
		{"tts", func(t *testing.T) (*rig, *fakeSession) {
			c := newFakeConcierge("The voice issue is in review since this morning.")
			c.hold = true
			r := newRig(t, c)
			sess := r.ask("what is happening?")
			waitSaid(t, sess)
			r.waitSpeaking()
			return r, sess
		}},
		{"gptlive", driving},
		{"dgagent", driving},
	}
}

// TestStopRuleForEveryVoice fails if, for any voice, "Wispr, stop", a gate
// closing or two real words in 0.5 s over its speech does not interrupt it
// and cancel the voice input, or one word ("wait") does.
func TestStopRuleForEveryVoice(t *testing.T) {
	stops := map[string]struct {
		do    func(r *rig)
		stops bool
	}{
		"Wispr, stop":  {func(r *rig) { r.heard("Wispr, stop.", false, false) }, true},
		"gate closing": {func(r *rig) { r.closeGate() }, true},
		"two words":    {func(r *rig) { r.heard("hold on", false, false) }, true},
		"one word":     {func(r *rig) { r.heard("wait", false, false) }, false},
	}
	for _, v := range everyVoice() {
		for name, stop := range stops {
			t.Run(v.name+"/"+name, func(t *testing.T) {
				r, sess := v.start(t)
				started := speechStart(r)
				stop.do(r)
				interrupted := func() bool { i, _, _ := sess.state(); return !i.IsZero() }
				if !stop.stops {
					never(t, "one word stopped the voice", 200*time.Millisecond, func() bool { return interrupted() || voiceCutAfter(r, started) })
					return
				}
				eventually(t, "the voice interrupted", interrupted)
				// The exchange cut the voice input as it took the floor, before
				// its voice spoke: only a cancel after the speech started is the
				// stop's.
				if !voiceCutAfter(r, started) {
					t.Error("the voice input was not cancelled")
				}
			})
		}
	}
}

// TestBackchannelsDoNotStop fails if R11's backchannels stop the voice, by
// its session or at the speaker, or if two other words do not stop it.
func TestBackchannelsDoNotStop(t *testing.T) {
	cases := map[string]bool{
		"mm-hm okay": false, "uh-huh yeah": false, "yeah right": false, "okay okay": false,
		"yes sure": true,
	}
	for heard, stops := range cases {
		t.Run(heard, func(t *testing.T) {
			r, sess := everyVoice()[0].start(t)
			started := speechStart(r)
			r.heard(heard, false, false)
			interrupted := func() bool { i, _, _ := sess.state(); return !i.IsZero() }
			if stops {
				eventually(t, "two real words stopped the voice", interrupted)
				return
			}
			never(t, "backchannels stopped the voice", 200*time.Millisecond, func() bool { return interrupted() || voiceCutAfter(r, started) })
		})
	}
}

// TestReplayWindow fails if a held answer cut in its first ReplayWindow does
// not stay pending, or one cut later is not dropped and recorded as spoken.
func TestReplayWindow(t *testing.T) {
	t.Run("cut early stays pending", func(t *testing.T) {
		c := newFakeConcierge()
		r := newRig(t, c)
		c.answers <- PendingAnswer{TaskID: "t1", SessionID: "sess-1", Text: "It ships tomorrow.", AskedAt: time.Now()}
		waitSaid(t, r.waitSession())
		r.s.Stop("Jabra button")
		r.heard("Wispr, go ahead.", true, true)
		if got := waitSaid(t, r.waitSession()); got != "It ships tomorrow." {
			t.Fatalf("said %q", got)
		}
	})
	t.Run("cut late is dropped", func(t *testing.T) {
		c := newFakeConcierge()
		r := newRig(t, c)
		c.answers <- PendingAnswer{TaskID: "t1", SessionID: "sess-1", Text: "It ships tomorrow.", AskedAt: time.Now()}
		waitSaid(t, r.waitSession())
		time.Sleep(400 * time.Millisecond) // past the rig's 300 ms window
		r.s.Stop("Jabra button")
		eventually(t, "the cut answer recorded as spoken", func() bool { return slices.Equal(c.spokenIDs(), []string{"t1"}) })
		r.heard("Wispr, go ahead.", true, true)
		never(t, "a dropped answer spoke again", 400*time.Millisecond, func() bool { return len(r.voice.opened) > 0 })
	})
}

// TestGoAheadOrder fails if "Wispr, go ahead" does not speak the oldest held
// answer, or "Wispr, go ahead, <key>" does not pick the answer of that key.
func TestGoAheadOrder(t *testing.T) {
	c := newFakeConcierge()
	r := newRig(t, c)
	for i, key := range []string{"ABC-1", "ABC-2", "ABC-3"} {
		c.answers <- PendingAnswer{TaskID: fmt.Sprintf("t%d", i+1), SessionID: "sess-1", IssueKey: key, Text: "About " + key + "."}
		r.waitEvent("voice_answer_ready:sess-1")
	}
	r.heard("Wispr, go ahead, ABC three.", true, true)
	if got := waitSaid(t, r.waitSession()); got != "About ABC-3." {
		t.Fatalf("said %q, want the answer of the key named", got)
	}
	r.speaker.finish(audio.InputVoice)
	eventually(t, "the floor free", func() bool { r.s.mu.Lock(); defer r.s.mu.Unlock(); return r.s.ex == nil })
	r.heard("Wispr, go ahead.", true, true)
	if got := waitSaid(t, r.waitSession()); got != "About ABC-1." {
		t.Fatalf("said %q, want the oldest", got)
	}
}

// --- R10's exchange window, R7's never mind ---

// TestAFollowUpContinuesTheExchange fails if an utterance with no wake phrase
// right after Wispr's answer is not a new turn of the same exchange, or the
// exchange stays open with no follow-up.
func TestAFollowUpContinuesTheExchange(t *testing.T) {
	c := newFakeConcierge("The voice issue is in review.")
	r := newRig(t, c)
	sess := r.ask("what is happening with the voice issue?")
	waitSaid(t, sess)
	r.speaker.finish(audio.InputVoice)
	r.waitEvent("voice_spoken:sess-1")
	r.heard("And who holds it?", true, true)
	if got := r.waitTurn(); got != "And who holds it?" {
		t.Fatalf("follow-up turn %q", got)
	}
	if len(r.voice.opened) > 0 {
		t.Fatal("a follow-up opened a second voice")
	}
	waitSaid(t, sess)
	r.speaker.finish(audio.InputVoice)
	eventually(t, "the exchange ended with no follow-up", func() bool { _, _, closed := sess.state(); return closed })
}

// TestThanksEndsTheExchange fails if "Thanks, Wispr." in the follow-up window
// does not end the exchange, or goes to the Concierge as a question.
func TestThanksEndsTheExchange(t *testing.T) {
	c := newFakeConcierge("The voice issue is in review.")
	r := newRigWith(t, c, func(cfg *SupervisorConfig, _ *rig) { cfg.FollowUpWait = time.Minute })
	sess := r.ask("what is happening?")
	waitSaid(t, sess)
	r.speaker.finish(audio.InputVoice)
	r.waitEvent("voice_spoken:sess-1")
	r.heard("Thanks, Wispr.", true, true)
	eventually(t, "the exchange ended", func() bool { _, _, closed := sess.state(); return closed })
	if len(c.asked) > 0 {
		t.Fatal("a thank-you went to the Concierge")
	}
}

// TestNeverMindCancelsTheNewestQuestion fails if "Wispr, never mind" does not
// ask the Concierge to cancel the session's newest question, or is asked as a
// question.
func TestNeverMindCancelsTheNewestQuestion(t *testing.T) {
	c := newFakeConcierge()
	r := newRig(t, c)
	r.heard("Wispr, never mind.", true, true)
	eventually(t, "the cancel", func() bool { return slices.Equal(c.neverMindCalls(), []string{"sess-1"}) })
	if len(c.asked) > 0 {
		t.Fatal("never mind went to the Concierge as a question")
	}
}

// TestASessionEndForgetsTheRecentTranscript fails if words of a meeting that
// ended reach the next meeting's exchange.
func TestASessionEndForgetsTheRecentTranscript(t *testing.T) {
	c := newFakeConcierge("fact")
	r := newRig(t, c)
	r.heard("words of the last meeting", true, true)
	r.s.SessionEnded("sess-1")
	r.ask("what is happening?")
	for _, turn := range c.exchanges[0].Recent {
		if turn.Text == "words of the last meeting" {
			t.Fatal("the next meeting's exchange got the last meeting's words")
		}
	}
}

// TestTimingEvents fails if an exchange and a held answer do not report the
// timing events the acceptance driver reads.
func TestTimingEvents(t *testing.T) {
	c := newFakeConcierge("The voice issue is in review.")
	r := newRig(t, c)
	sess := r.ask("what is happening?")
	waitSaid(t, sess)
	r.speaker.finish(audio.InputVoice)
	r.waitEvent("voice_spoken:sess-1")
	// The whole exchange can end within a millisecond here, before the wake
	// sound's first frame or before the speaker's events reach the
	// Supervisor; a stop then rightly cuts them. So the stop waits for them.
	waitTimings(t, r, "wake_heard", "wake_chime_queued", "turn_end", "first_audio", "fact_audio", "speech_end")
	r.s.Stop("Jabra button")
	c.answers <- PendingAnswer{TaskID: "t1", SessionID: "sess-1", Text: "x"}
	waitTimings(t, r, "stop", "answer_ready", "answer_chime")
}

// TestFactAudioOfASayThatPlayedBeforeItReturned fails if the first fact's
// audio goes unreported when its speech started and ended before Say
// returned, as a vendor voice's Say may.
func TestFactAudioOfASayThatPlayedBeforeItReturned(t *testing.T) {
	c := newFakeConcierge("The voice issue is in review.")
	r := newRigWith(t, c, func(cfg *SupervisorConfig, r *rig) {
		cfg.Voice = func() (Voice, error) { return playedVoice{r: r}, nil }
	})
	r.ask("what is happening?")
	waitTimings(t, r, "first_audio", "speech_end", "fact_audio")
}

// TestFactAudioIsTheSpeechStart fails if the first fact's audio is reported
// when its Say returns rather than when its speech started. Voice A's Say
// returns once the last of its audio is queued, about as long as it plays
// (end-to-end run 8 reported a fact 7 s late).
func TestFactAudioIsTheSpeechStart(t *testing.T) {
	c := newFakeConcierge("The voice issue is in review.")
	r := newRigWith(t, c, func(cfg *SupervisorConfig, r *rig) {
		cfg.Voice = func() (Voice, error) { return slowSayVoice{r: r}, nil }
	})
	r.ask("what is happening?")
	waitTimings(t, r, "first_audio", "fact_audio")
	if late := r.timingOf("fact_audio").Sub(r.timingOf("first_audio")); late > 50*time.Millisecond {
		t.Fatalf("fact_audio came %v after the speech started, want its start", late)
	}
}

// slowSayVoice opens the rig's fake sessions, and returns from each Say
// 300 ms after its speech started, while it still plays.
type slowSayVoice struct{ r *rig }

func (v slowSayVoice) Open(ctx context.Context, x *Exchange) (Session, error) {
	s, err := v.r.voice.Open(ctx, x)
	if err != nil {
		return nil, err
	}
	return slowSaySession{Session: s, r: v.r}, nil
}

type slowSaySession struct {
	Session
	r *rig
}

func (p slowSaySession) Say(ctx context.Context, text string, mode SayMode) error {
	if err := p.Session.Say(ctx, text, mode); err != nil {
		return err
	}
	eventually(p.r.t, "the speech started", func() bool {
		p.r.s.mu.Lock()
		defer p.r.s.mu.Unlock()
		return p.r.s.voiceSpeaking
	})
	time.Sleep(300 * time.Millisecond)
	return nil
}

// playedVoice opens the rig's fake sessions, and returns from each Say only
// once its speech has played.
type playedVoice struct{ r *rig }

func (v playedVoice) Open(ctx context.Context, x *Exchange) (Session, error) {
	s, err := v.r.voice.Open(ctx, x)
	if err != nil {
		return nil, err
	}
	return playedSession{Session: s, r: v.r}, nil
}

type playedSession struct {
	Session
	r *rig
}

func (p playedSession) Say(ctx context.Context, text string, mode SayMode) error {
	if err := p.Session.Say(ctx, text, mode); err != nil {
		return err
	}
	speaking := func() bool {
		p.r.s.mu.Lock()
		defer p.r.s.mu.Unlock()
		return p.r.s.voiceSpeaking
	}
	eventually(p.r.t, "the speech started", speaking)
	p.r.speaker.finish(audio.InputVoice)
	eventually(p.r.t, "the speech ended", func() bool { return !speaking() })
	return nil
}

// waitTimings waits until the rig saw each timing event of want, and names
// those it did not see.
func waitTimings(t *testing.T, r *rig, want ...string) {
	t.Helper()
	missing := func() []string {
		got := r.timingEvents()
		var out []string
		for _, w := range want {
			if !slices.Contains(got, w) {
				out = append(out, w)
			}
		}
		return out
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(missing()) > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if m := missing(); len(m) > 0 {
		t.Fatalf("missing timing events %v; got %v", m, r.timingEvents())
	}
}

// --- c-voices ---

// TestEachVoicePassesTheScriptedExchange fails if any voice, voice A or a
// voice that drives its own turn with the Concierge's tools, misses a step of
// one script: a call and its fast answer, a follow-up heard as the next turn,
// a stop, a pause in the middle of an answer, and a holder's answer on
// invitation.
func TestEachVoicePassesTheScriptedExchange(t *testing.T) {
	t.Run("tts", func(t *testing.T) {
		c := newFakeConcierge("The voice issue is in review.")
		r := newRig(t, c)
		sess := r.ask("what is happening with the voice issue?")
		waitSaid(t, sess)
		r.speaker.finish(audio.InputVoice)
		r.waitEvent("voice_spoken:sess-1") // the answer played: the follow-up window opens
		r.heard("And who holds it?", true, true)
		r.waitTurn()
		waitSaid(t, sess)
		r.heard("Wispr, stop.", false, false)
		eventually(t, "the stop", func() bool { _, _, closed := sess.state(); return closed })

		c.setHold(true) // the follow-up's answer may still be running
		paused := r.ask("what changed today?")
		waitSaid(t, paused)
		asked := time.Now()
		r.closeGate()
		eventually(t, "the pause ended the exchange", func() bool { _, _, closed := paused.state(); return closed })

		c.answers <- PendingAnswer{TaskID: "t1", SessionID: "sess-1", Text: "It ships tomorrow.", AskedAt: asked}
		r.waitEvent("voice_answer_ready:sess-1")
		r.heard("Wispr, go ahead.", true, true)
		if got := waitSaid(t, r.waitSession()); got != "It ships tomorrow." {
			t.Fatalf("said %q", got)
		}
	})
	for _, name := range []string{"gptlive", "dgagent"} {
		t.Run(name, func(t *testing.T) {
			c := newFakeConcierge("The voice issue is in review.")
			r := newDrivingRigWith(t, time.Minute, c)
			r.heard("Wispr, what is happening with the voice issue?", true, true)
			sess := r.waitSession()
			vendorTurn := func(question string) {
				t.Helper()
				out, err := sess.x.Tools.Run(t.Context(), ToolCall{ID: "d1", Name: AnswerTool, Args: json.RawMessage(fmt.Sprintf(`{"question":%q}`, question))})
				if err != nil {
					t.Fatalf("the answer tool: %v", err)
				}
				var res AnswerResult
				if err := json.Unmarshal(out, &res); err != nil || res.Say == "" {
					t.Fatalf("the answer tool returned %s", out)
				}
				speakAs(t, sess, res.Say)
			}
			vendorTurn("what is happening with the voice issue?")
			r.speaker.finish(audio.InputVoice) // the answer plays to its end
			r.heard("And who holds it?", true, true)
			r.s.mu.Lock()
			turn := r.s.turnOf(r.s.ex)
			r.s.mu.Unlock()
			if turn != "And who holds it?" {
				t.Fatalf("the follow-up's turn is %q, want %q", turn, "And who holds it?")
			}
			vendorTurn("and who holds it?")
			r.heard("Wispr, stop.", false, false)
			eventually(t, "the stop", func() bool { _, _, closed := sess.state(); return closed })

			r.heard("Wispr, what changed today?", true, true)
			paused := r.waitSession()
			speakAs(t, paused, "Today the plan changed.")
			asked := time.Now()
			r.closeGate()
			eventually(t, "the pause ended the exchange", func() bool { _, _, closed := paused.state(); return closed })

			c.answers <- PendingAnswer{TaskID: "t1", SessionID: "sess-1", Text: "It ships tomorrow.", AskedAt: asked}
			r.waitEvent("voice_answer_ready:sess-1")
			r.heard("Wispr, go ahead.", true, true)
			if got := waitSaid(t, r.waitSession()); got != "It ships tomorrow." {
				t.Fatalf("said %q", got)
			}
		})
	}
}

// TestEngineChangesAtNextExchange fails if a change of voice during an
// exchange changes that exchange's voice, or the next exchange keeps the old.
func TestEngineChangesAtNextExchange(t *testing.T) {
	c := newFakeConcierge("The voice issue is in review.")
	second := newFakeVoice()
	var mu sync.Mutex
	useSecond := false
	r := newRigWith(t, c, func(cfg *SupervisorConfig, r *rig) {
		cfg.Voice = func() (Voice, error) {
			mu.Lock()
			defer mu.Unlock()
			if useSecond {
				return second, nil
			}
			return r.voice, nil
		}
	})
	sess := r.ask("what is happening?")
	waitSaid(t, sess)
	mu.Lock()
	useSecond = true
	mu.Unlock()
	r.speaker.finish(audio.InputVoice)
	r.waitEvent("voice_spoken:sess-1")       // the answer played: the follow-up window opens
	r.heard("And who holds it?", true, true) // a follow-up of the same exchange
	r.waitTurn()
	waitSaid(t, sess)
	if len(second.opened) > 0 {
		t.Fatal("the change took over the exchange in progress")
	}
	r.speaker.finish(audio.InputVoice)
	eventually(t, "the exchange ended", func() bool { _, _, closed := sess.state(); return closed })
	r.heard("Wispr, what changed today?", true, true)
	select {
	case <-second.opened:
	case <-time.After(testWait):
		t.Fatal("the next exchange did not use the new voice")
	}
}

// TestVoiceDisabled fails if, with the voice off, a call opens an exchange,
// sends a /ws event, plays the wake sound or reaches the Concierge, or if the
// start sound and a failed red check's error sound do not play.
func TestVoiceDisabled(t *testing.T) {
	c := newFakeConcierge("fact")
	r := newRig(t, c)
	r.setEnabled(false)
	r.heard("Wispr, what is happening?", true, true)
	never(t, "a call opened an exchange with the voice off", 400*time.Millisecond, func() bool {
		return len(r.voice.opened) > 0 || len(r.events.all()) > 0 || len(c.heardCalls()) > 0 || len(r.speaker.soundsPlayed()) > 0
	})
	r.s.Started()
	r.s.RedCheckFailed()
	eventually(t, "the start and error sounds", func() bool {
		got := r.speaker.soundsPlayed()
		return slices.Contains(got, "start") && slices.Contains(got, "error")
	})
	r.setEnabled(true)
	r.ask("what is happening?")
}

// TestFactAudioFollowsTheVoicesTextAudio fails if, for a voice that
// synthesises a text while the one before plays (voice A) and so returns from
// Say before the text plays, the first fact's audio is timed at its Say rather
// than when the voice reports the fact's text begins to play (EventTextAudio).
func TestFactAudioFollowsTheVoicesTextAudio(t *testing.T) {
	c := newFakeConcierge("The issue is in review.")
	c.acks, c.ackMode = []string{"I took ABC-12."}, Lead
	var v *pipelinedVoice
	r := newRigWith(t, c, func(cfg *SupervisorConfig, r *rig) {
		v = &pipelinedVoice{r: r, lead: 300 * time.Millisecond}
		cfg.Voice = func() (Voice, error) { return v, nil }
		// The fake plays its second text 300 ms after the first with nothing
		// queued between, which voice A does not do; the window keeps the
		// exchange open meanwhile.
		cfg.FollowUpWait = time.Minute
	})
	r.ask("what is happening with the voice work?")
	waitTimings(t, r, "fact_audio")
	factAt := v.playedAt("The issue is in review.")
	if diff := r.timingOf("fact_audio").Sub(factAt); diff < -50*time.Millisecond || diff > 50*time.Millisecond {
		t.Fatalf("fact_audio came %v from the fact's text audio, want at it", diff.Round(time.Millisecond))
	}
}

// pipelinedVoice opens the rig's fake sessions as voice A now behaves: Say
// returns at once, and each text plays in order, the first for lead, with
// EventTextAudio as it begins.
type pipelinedVoice struct {
	r    *rig
	lead time.Duration
	mu   sync.Mutex
	at   map[string]time.Time
}

func (v *pipelinedVoice) playedAt(text string) time.Time {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.at[text]
}

func (v *pipelinedVoice) Open(ctx context.Context, x *Exchange) (Session, error) {
	s, err := v.r.voice.Open(ctx, x)
	if err != nil {
		return nil, err
	}
	p := &pipelinedSession{Session: s, v: v, texts: make(chan sayText, 8), events: make(chan Event, 64), played: make(chan struct{})}
	go p.forward()
	go p.play(ctx)
	return p, nil
}

type sayText struct {
	text string
	mode SayMode
}

type pipelinedSession struct {
	Session
	v      *pipelinedVoice
	texts  chan sayText
	events chan Event
	played chan struct{} // closed when play ends: events closes after it
}

func (*pipelinedSession) ReportsTextAudio() bool { return true }
func (p *pipelinedSession) Events() <-chan Event { return p.events }

func (p *pipelinedSession) Say(_ context.Context, text string, mode SayMode) error {
	p.texts <- sayText{text, mode}
	return nil
}

// forward passes the rig session's events on; once they end (Close), it
// waits for play to stop, then closes Events.
func (p *pipelinedSession) forward() {
	for e := range p.Session.Events() {
		p.events <- e
	}
	<-p.played
	close(p.events)
}

func (p *pipelinedSession) play(ctx context.Context) {
	defer close(p.played)
	first := true
	for {
		select {
		case s := <-p.texts:
			if !first {
				select {
				case <-time.After(p.v.lead): // the text before plays
				case <-ctx.Done():
					return
				}
			}
			first = false
			p.v.mu.Lock()
			if p.v.at == nil {
				p.v.at = map[string]time.Time{}
			}
			p.v.at[s.text] = time.Now()
			p.v.mu.Unlock()
			p.events <- Event{Kind: EventTextAudio, Text: s.text}
			_ = p.Session.Say(ctx, s.text, s.mode)
		case <-ctx.Done():
			return
		}
	}
}
