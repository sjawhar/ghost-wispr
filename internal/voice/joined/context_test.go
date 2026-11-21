package joined

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/envoy"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// The tests here check that a join or a pointer commits only to the meeting
// the recorder holds open at that moment, and that every effect on a
// membership runs under the membership's own context, so its end cancels the
// effect wherever it has got to.

// TestAJoinOrPointerCommitsOnlyToTheCurrentMeeting fails if a join and a
// final segment of a meeting, both in flight while that meeting ended, was
// deleted, and the roster's record of ended meetings forgot it under older
// deletions, then commit to it: a membership of the deleted meeting, or a
// pointer to its words. (ReviewPR122b's
// TestReviewGPrunedEndCannotReviveDeletedMeeting.)
func TestAJoinOrPointerCommitsOnlyToTheCurrentMeeting(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "unit.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.CreateSession("sess-1", meetingStart); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSegment("sess-1", transcribe.Segment{Text: "ABC-12 is late", StartTime: 297, EndTime: 300, Timestamp: meetingStart.Add(300 * time.Second)}); err != nil {
		t.Fatal(err)
	}
	joinEntered, joinRelease := make(chan struct{}), make(chan struct{})
	segmentEntered, segmentRelease := make(chan struct{}), make(chan struct{})
	var holdTitle, holdSegment atomic.Bool
	holdTitle.Store(true)
	r := newRig(t, func(cfg *Config) {
		now := cfg.Now
		cfg.Now = func() time.Time {
			at := now()
			if holdSegment.CompareAndSwap(true, false) {
				close(segmentEntered)
				<-segmentRelease
			}
			return at
		}
		cfg.Started = func(id string) time.Time {
			s, err := store.GetSession(id)
			if err != nil {
				return time.Time{}
			}
			return s.StartedAt
		}
		cfg.Title = func(_ context.Context, key string) (string, error) {
			if key == "ABC-12" && holdTitle.CompareAndSwap(true, false) {
				close(joinEntered)
				<-joinRelease
			}
			return titles[key], nil
		}
	})
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		_, _ = r.roster.Deliver(context.Background(), &envoy.Message{EventID: "old-join", SourceSession: "agent-1", Text: "ghost-wispr join ABC-12", IssuedAt: r.clock.Now()})
	}()
	<-joinEntered
	holdSegment.Store(true)
	segmented := make(chan struct{})
	go func() {
		defer close(segmented)
		r.roster.OnSegment(&voice.Segment{SessionID: "sess-1", Text: "ABC-12 is late", From: meetingStart.Add(297 * time.Second), To: meetingStart.Add(300 * time.Second)})
	}()
	<-segmentEntered
	if err := store.EndSession("sess-1", r.clock.Now(), ""); err != nil {
		t.Fatal(err)
	}
	r.setSession("sess-2")
	r.roster.SessionEnded("sess-1")
	if _, err := store.DeleteSession("sess-1", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	r.roster.SessionsDeleted([]string{"sess-1"})
	r.join(t, "agent-2", "XYZ-9")
	// A retention sweep deletes enough older recordings to push sess-1 out
	// of the roster's record of ended meetings.
	older := make([]string, maxEnded)
	for i := range older {
		older[i] = fmt.Sprintf("historical-recording-%d", i)
	}
	r.roster.SessionsDeleted(older)
	r.roster.mu.Lock()
	forgot := !slices.Contains(r.roster.ended, "sess-1")
	r.roster.mu.Unlock()
	if !forgot {
		t.Fatal("control: the record of ended meetings still holds sess-1")
	}
	close(joinRelease)
	<-joined
	close(segmentRelease)
	<-segmented
	r.roster.wg.Wait()
	got := r.roster.Joined()
	if !slices.ContainsFunc(got, func(m Member) bool { return m.Agent == "agent-2" && m.SessionID == "sess-2" }) {
		t.Fatalf("control: the current meeting's member is missing: %+v", got)
	}
	if slices.ContainsFunc(got, func(m Member) bool { return m.SessionID == "sess-1" }) {
		t.Fatalf("the deleted meeting gained a membership: %+v", got)
	}
	if p := r.pointers("agent-1"); len(p) != 0 {
		t.Fatalf("a pointer to the deleted meeting's words was sent: %q", p[0].Message)
	}
}

// heldWelcomeAgents is the unit's Envoy session with the first welcome held
// before it reaches the transport, until released or its context ends.
type heldWelcomeAgents struct {
	*fakeAgents
	hold             atomic.Bool
	entered, release chan struct{}
}

func (a *heldWelcomeAgents) Send(ctx context.Context, m *envoy.SendRequest) (envoy.SendResult, error) {
	if strings.HasPrefix(m.Message, "You joined") && a.hold.CompareAndSwap(true, false) {
		close(a.entered)
		select {
		case <-a.release:
		case <-ctx.Done():
			return envoy.SendResult{}, ctx.Err()
		}
	}
	if err := ctx.Err(); err != nil {
		return envoy.SendResult{}, err
	}
	return a.fakeAgents.Send(ctx, m)
}

// TestAWelcomeRunsUnderItsMembership fails if a welcome on its way to the
// transport when its meeting ends still goes out: it must run under its
// membership's context, not the delivery's. A live membership's welcome is
// the control. (ReviewPR122b's TestReviewGWelcomeUsesMembershipCancellation.)
func TestAWelcomeRunsUnderItsMembership(t *testing.T) {
	r := newRig(t, nil)
	a := &heldWelcomeAgents{fakeAgents: r.agents, entered: make(chan struct{}), release: make(chan struct{})}
	a.hold.Store(true)
	r.roster.SetAgents(a)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = r.roster.Deliver(context.Background(), &envoy.Message{EventID: "join", SourceSession: "agent-1", Text: "ghost-wispr join ABC-12", IssuedAt: r.clock.Now()})
	}()
	<-a.entered
	r.roster.SessionEnded("sess-1")
	close(a.release)
	<-done
	r.roster.wg.Wait()
	for _, m := range r.agents.to("agent-1") {
		if strings.HasPrefix(m.Message, "You joined") {
			t.Fatalf("a welcome reached the transport after its meeting ended: %q", m.Message)
		}
	}
	r.setSession("sess-2")
	r.join(t, "agent-2", "XYZ-9")
	if !slices.ContainsFunc(r.agents.to("agent-2"), func(m envoy.SendRequest) bool { return strings.HasPrefix(m.Message, "You joined") }) {
		t.Fatal("control: a live membership's welcome is missing")
	}
}

// frameSpeaker is a speaker whose voice input records every frame that
// reaches it, refused once Cancel ran; the chime input plays nothing.
type frameSpeaker struct {
	mu        sync.Mutex
	playbacks []*framePlayback
	frames    chan []byte
	chime     soundSpeaker
}

func (s *frameSpeaker) Begin(input string) voice.Playback {
	if input != audio.InputVoice {
		return s.chime.Begin(input)
	}
	p := &framePlayback{s: s}
	s.mu.Lock()
	s.playbacks = append(s.playbacks, p)
	s.mu.Unlock()
	return p
}

func (s *frameSpeaker) Cancel(input string) {
	if input != audio.InputVoice {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.playbacks {
		p.canceled.Store(true)
	}
}

func (s *frameSpeaker) OnSpeech(func(audio.SpeechEvent)) func() { return func() {} }
func (s *frameSpeaker) FlushSpeech()                            {}

type framePlayback struct {
	s        *frameSpeaker
	canceled atomic.Bool
}

func (p *framePlayback) Write(ctx context.Context, b []byte, _ audio.AudioFormat) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.canceled.Load() {
		return errors.New("playback cancelled")
	}
	p.s.frames <- append([]byte(nil), b...)
	return nil
}

func (p *framePlayback) Drain(ctx context.Context) error { return ctx.Err() }

// slowVoice opens sessions whose Say waits, as a vendor's synthesis does,
// before its first frame: it hands the test its context and waits for it.
type slowVoice struct {
	entered chan context.Context
	release chan struct{}
}

func (v slowVoice) Open(ctx context.Context, x *voice.Exchange) (voice.Session, error) {
	base, err := (sayingVoice{says: make(chan string, 8)}).Open(ctx, x)
	if err != nil {
		return nil, err
	}
	return &slowSay{Session: base, out: x.Out, entered: v.entered, release: v.release}, nil
}

type slowSay struct {
	voice.Session
	out     voice.Output
	entered chan context.Context
	release chan struct{}
}

func (s *slowSay) Say(ctx context.Context, _ string, _ voice.SayMode) error {
	s.entered <- ctx
	select {
	case <-s.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	p := s.out.Begin()
	if err := p.Write(ctx, []byte{1, 0, 2, 0, 3, 0, 4, 0}, voice.ChimeFormat); err != nil {
		return err
	}
	return p.Drain(ctx)
}

// TestAHandsSpeechEndsWithItsMembership fails if a leave that completes while
// the voice waits to synthesise a hand's words still lets their first frame
// reach the speaker; a live membership's words are the control.
// (ReviewPR122b's TestReviewGLeaveCancelsHandBeforeItsFirstVoiceFrame.)
func TestAHandsSpeechEndsWithItsMembership(t *testing.T) {
	for _, leave := range []bool{false, true} {
		t.Run(map[bool]string{false: "the live control", true: "a leave"}[leave], func(t *testing.T) {
			sp := &frameSpeaker{frames: make(chan []byte, 8)}
			entered, release := make(chan context.Context, 1), make(chan struct{})
			var roster atomic.Pointer[Roster]
			s := voice.NewSupervisor(&voice.SupervisorConfig{
				Voice:     func() (voice.Voice, error) { return slowVoice{entered: entered, release: release}, nil },
				Concierge: quietConcierge{}, Speaker: sp, WakePhrases: []string{"Wispr"},
				Session: func() string { return "sess-1" }, Red: func() bool { return true },
				SoundGap: 50 * time.Millisecond, SilenceWait: 100 * time.Millisecond,
				HandDone: func(h *voice.Hand, o voice.HandOutcome) {
					if r := roster.Load(); r != nil {
						r.HandDone(h, o)
					}
				},
				Logf: t.Logf,
			})
			r := newRig(t, func(cfg *Config) {
				cfg.Now = nil
				cfg.Floor = s
				cfg.Session = func() string { return "sess-1" }
			})
			t.Cleanup(s.Close)
			roster.Store(r.roster)
			r.clock.set(time.Now())
			r.join(t, "agent-1", "ABC-12")
			r.deliver(t, "agent-1", "ghost-wispr floor\nThe release is ready.")
			(&supervisorRig{t: t, s: s}).heard("Wispr, go ahead.")
			select {
			case <-entered:
			case <-time.After(testWait):
				t.Fatal("control: the voice never began to say the words")
			}
			if len(sp.frames) != 0 {
				t.Fatal("control: a frame came before the voice was let go")
			}
			if leave {
				r.deliver(t, "agent-1", "ghost-wispr leave")
				if len(r.roster.Joined()) != 0 {
					t.Fatal("control: the leave did not end the membership")
				}
			}
			close(release)
			var frame []byte
			select {
			case frame = <-sp.frames:
			case <-time.After(200 * time.Millisecond):
			}
			if leave && len(frame) > 0 {
				t.Fatal("a frame of the words reached the speaker after the membership's leave completed")
			}
			if !leave && len(frame) == 0 {
				t.Fatal("control: a live membership's words reached no speaker")
			}
		})
	}
}
