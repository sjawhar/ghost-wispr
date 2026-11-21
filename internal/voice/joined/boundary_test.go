package joined

import (
	"context"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/envoy"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// lockWatch records, for each hook the roster calls out through, how often
// it was called and how often with the roster's lock held: a hook that
// finds the lock taken by TryLock ran under it, since the tests here run one
// step at a time and wait for its sends before the next.
type lockWatch struct {
	roster atomic.Pointer[Roster]
	mu     sync.Mutex
	calls  map[string]int
	held   map[string]int
}

func (w *lockWatch) check(hook string) {
	r := w.roster.Load()
	if r == nil {
		return
	}
	held := !r.mu.TryLock()
	if !held {
		r.mu.Unlock()
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls[hook]++
	if held {
		w.held[hook]++
	}
}

// watchedAgents is the unit's Envoy session with its calls watched.
type watchedAgents struct {
	*fakeAgents
	w *lockWatch
}

func (a *watchedAgents) Lookup(ctx context.Context, id string) (envoy.SessionInfo, error) {
	a.w.check("Agents.Lookup")
	return a.fakeAgents.Lookup(ctx, id)
}

func (a *watchedAgents) Send(ctx context.Context, m *envoy.SendRequest) (envoy.SendResult, error) {
	a.w.check("Agents.Send")
	return a.fakeAgents.Send(ctx, m)
}

// TestNoHookRunsUnderTheRosterLock fails if the roster calls any of its
// hooks (the Supervisor's RaiseHand and LowerHands, Envoy's Lookup and Send,
// the recording session's start, Dispatch's title, the /ws notice, the
// keyterms or the clock) while it holds its own lock, on any path: a join, a
// repeated join, a floor request, a pointer and a queued one, a sweep, a
// hand's outcome, a leave, a meeting's end and deletion, and the voice
// stopping. A hook under the lock is a deadlock waiting for a hook that calls
// back (HandDone, Segment). The one read the roster makes under its lock is
// the recorder's current session (Config.Session), at each commit, which
// returns at once and calls neither the roster nor the Supervisor; it must
// still be read on these paths, and is reported.
func TestNoHookRunsUnderTheRosterLock(t *testing.T) {
	w := &lockWatch{calls: map[string]int{}, held: map[string]int{}}
	r := newRig(t, func(cfg *Config) {
		session, started, title, notify, keyterms, now := cfg.Session, cfg.Started, cfg.Title, cfg.Notify, cfg.Keyterms, cfg.Now
		cfg.Session = func() string { w.check("Session"); return session() }
		cfg.Started = func(id string) time.Time { w.check("Started"); return started(id) }
		cfg.Title = func(ctx context.Context, key string) (string, error) { w.check("Title"); return title(ctx, key) }
		cfg.Notify = func(kind, id string) { w.check("Notify"); notify(kind, id) }
		cfg.Keyterms = func(keys []string) { w.check("Keyterms"); keyterms(keys) }
		cfg.Now = func() time.Time { w.check("Now"); return now() }
	})
	r.floor.beforeRaise = func(*voice.Hand) { w.check("Floor.RaiseHand") }
	r.floor.beforeLower = func(string) { w.check("Floor.LowerHands") }
	agents := &watchedAgents{fakeAgents: r.agents, w: w}
	r.roster.SetAgents(agents)
	w.roster.Store(r.roster)
	step := func(f func()) {
		f()
		r.roster.wg.Wait()
	}

	step(func() { r.join(t, "agent-1", "ABC-12") })
	step(func() { r.deliver(t, "agent-1", "ghost-wispr join ABC-12") })
	step(func() { r.deliver(t, "agent-1", "ghost-wispr floor\nThe release is ready.") })
	step(func() { r.roster.OnSegment(r.segment("ABC-12 is late", false)) })
	step(func() { r.clock.advance(15 * time.Second); r.roster.OnSegment(r.segment("ABC-12 again", false)) })
	step(func() { r.clock.advance(time.Minute); r.roster.Tick() })
	step(func() { r.roster.Sweep(context.Background()) })
	step(func() {
		hand := r.floor.hands()[0]
		r.roster.HandDone(hand, voice.HandExpired)
	})
	step(func() { r.deliver(t, "agent-1", "ghost-wispr leave") })
	step(func() { r.join(t, "agent-1", "ABC-12") })
	step(func() { r.roster.SessionEnded("sess-1") })
	step(func() { r.setSession("sess-2"); r.join(t, "agent-1", "ABC-12") })
	step(func() { r.roster.SessionsDeleted([]string{"sess-2"}) })
	step(func() { r.setSession("sess-3"); r.join(t, "agent-1", "ABC-12") })
	step(func() { r.roster.SetAgents(nil) })

	w.mu.Lock()
	defer w.mu.Unlock()
	var missing, held []string
	for _, hook := range []string{"Floor.RaiseHand", "Floor.LowerHands", "Agents.Lookup", "Agents.Send", "Session", "Started", "Title", "Notify", "Keyterms", "Now"} {
		if w.calls[hook] == 0 {
			missing = append(missing, hook)
		}
	}
	for hook, n := range w.held {
		t.Logf("%s: %d of %d calls under the roster's lock", hook, n, w.calls[hook])
		if hook != "Session" {
			held = append(held, hook)
		}
	}
	sort.Strings(held)
	if len(missing) > 0 {
		t.Fatalf("control: the run never called %s, so it cannot tell whether they run under the lock", strings.Join(missing, ", "))
	}
	if len(held) > 0 {
		t.Fatalf("hooks called with the roster's lock held: %s", strings.Join(held, ", "))
	}
	if w.held["Session"] == 0 {
		t.Fatal("the roster never read the recorder's session under its lock: no commit is checked against the meeting held open at that moment")
	}
}

// TestAnOldStopCannotClearANewEnvoySession fails if a SetAgents(nil) still
// in its cleanup when SetAgents(new) opens a new Envoy session, and a member
// joins under it, then takes the new session or the new member when it
// finishes; or if its notices go out through the new session. (ReviewPR122b's
// TestReviewEOldStopCannotClearNewEnvoyEpoch.)
func TestAnOldStopCannotClearANewEnvoySession(t *testing.T) {
	r := newRig(t, nil)
	r.join(t, "agent-1", "ABC-12")
	entered, release := make(chan struct{}), make(chan struct{})
	var hold atomic.Bool
	hold.Store(true)
	r.floor.beforeLower = func(string) {
		if hold.CompareAndSwap(true, false) {
			close(entered)
			<-release
		}
	}
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		r.roster.SetAgents(nil)
	}()
	<-entered
	next := newFakeAgents()
	r.roster.SetAgents(next)
	r.deliver(t, "agent-2", "ghost-wispr join XYZ-9")
	close(release)
	<-stopped
	r.roster.wg.Wait()
	if got := r.roster.currentAgents(); got != next {
		t.Fatalf("the old stop cleared the new Envoy session: the roster holds %T", got)
	}
	if got := r.roster.Joined(); len(got) != 1 || got[0].Agent != "agent-2" {
		t.Fatalf("roster %+v, want agent-2, which joined under the new session", got)
	}
	for _, m := range next.to("agent-1") {
		if strings.Contains(m.Message, "voice stopped") {
			t.Fatal("the old stop's notice to agent-1 went out through the new Envoy session")
		}
	}
	if got := r.agents.to("agent-1"); len(got) == 0 || !strings.Contains(got[len(got)-1].Message, "voice stopped") {
		t.Fatalf("agent-1 was not told, through the session it joined under, that the voice stopped: %+v", got)
	}
}
