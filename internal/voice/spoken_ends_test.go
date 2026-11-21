package voice

import (
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

// endReasons is the reason of each call of endLocked in supervisor.go, read
// from its source: a string literal; the format of a fmt.Sprintf; or, for a
// reason its caller passes in, "<function>(reason)". It fails t on a call
// whose reason is none of those, so no end can hide from spokenEnds.
func endReasons(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "supervisor.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var reasons []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "endLocked" || len(call.Args) != 3 {
				return true
			}
			var reason string
			switch arg := call.Args[2].(type) {
			case *ast.BasicLit:
				reason, _ = strconv.Unquote(arg.Value)
			case *ast.Ident:
				reason = fn.Name.Name + "(" + arg.Name + ")"
			case *ast.CallExpr:
				if f, ok := arg.Fun.(*ast.SelectorExpr); ok && f.Sel.Name == "Sprintf" && len(arg.Args) > 0 {
					if lit, ok := arg.Args[0].(*ast.BasicLit); ok {
						reason, _ = strconv.Unquote(lit.Value)
					}
				}
			}
			if reason == "" {
				t.Errorf("%s: endLocked's reason is neither a string, a fmt.Sprintf of one, nor a name: spokenEnds cannot list it", fset.Position(call.Pos()))
			} else if !slices.Contains(reasons, reason) {
				reasons = append(reasons, reason)
			}
			return true
		})
	}
	if len(reasons) < 15 {
		t.Fatalf("control: UNKNOWN: %d end reasons read from supervisor.go, want every one (about 20)", len(reasons))
	}
	return reasons
}

// afterSpeech is one way an exchange ends, after its voice, which drives its
// own turn, has spoken: the speaker played its speech to the end, and the
// voice's own report that it ended trails by a second.
type afterSpeech struct {
	// end ends the exchange that way; nil when it cannot end that way.
	end func(t *testing.T, r *rig, c *fakeConcierge, sess *fakeSession)
	// cannot says why a driving voice's exchange cannot end so once its
	// voice spoke.
	cannot string
	// idle and followUp are the rig's IdleWait and FollowUpWait; 0 is a
	// minute, so neither ends the exchange first. limit is exchangeMax; 0
	// leaves it.
	idle, followUp, limit time.Duration
}

// spokenEnds is how a driving voice's exchange ends after its voice spoke,
// for each reason endLocked is called with (endReasons).
var spokenEnds = map[string]afterSpeech{
	"stopLocked(reason)": {end: func(t *testing.T, r *rig, _ *fakeConcierge, _ *fakeSession) {
		r.s.Stop("the button")
	}},
	"Wispr, never mind": {end: func(t *testing.T, r *rig, _ *fakeConcierge, _ *fakeSession) {
		r.heard("Wispr, never mind.", true, true)
	}},
	"thanks": {end: func(t *testing.T, r *rig, _ *fakeConcierge, _ *fakeSession) {
		r.heard("Thanks, Wispr.", true, true)
	}},
	"its session was deleted": {end: func(t *testing.T, r *rig, _ *fakeConcierge, _ *fakeSession) {
		r.s.SessionsDeleted([]string{"sess-1"})
	}},
	"the voice failed": {end: func(t *testing.T, _ *rig, _ *fakeConcierge, sess *fakeSession) {
		sess.fail(errors.New("the vendor's connection dropped"))
	}},
	"the voice ended by itself": {end: func(t *testing.T, _ *rig, _ *fakeConcierge, sess *fakeSession) {
		sess.die()
	}},
	"idle":         {end: func(*testing.T, *rig, *fakeConcierge, *fakeSession) {}, idle: 300 * time.Millisecond},
	"no follow-up": {end: func(*testing.T, *rig, *fakeConcierge, *fakeSession) {}, followUp: 300 * time.Millisecond},
	"it reached its limit of %v": {end: func(*testing.T, *rig, *fakeConcierge, *fakeSession) {},
		limit: 2 * time.Second},
	"side talk in the follow-up window": {end: func(t *testing.T, r *rig, _ *fakeConcierge, sess *fakeSession) {
		r.heard(sideTalk, true, true)
		out, err := sess.x.Tools.Run(t.Context(), ToolCall{ID: "side", Name: AnswerTool, Args: json.RawMessage(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		var result AnswerResult
		if err := json.Unmarshal(out, &result); err != nil || result.Say != "" {
			t.Fatalf("control: the side talk's answer is %s (%v), want nothing to say", out, err)
		}
	}},
	"a holder's answer takes the floor": {end: func(t *testing.T, r *rig, c *fakeConcierge, _ *fakeSession) {
		c.answers <- PendingAnswer{TaskID: "holder", SessionID: "sess-1", IssueKey: "ABC-12", Text: "It ships tomorrow.", AskedAt: time.Now()}
		if got := waitSaid(t, r.waitSession()); got != "It ships tomorrow." {
			t.Fatalf("control: the holder's answer said %q", got)
		}
	}},

	"Wispr, go ahead":         {cannot: "it ends only an exchange whose voice does not drive its turn"},
	"a new call":              {cannot: "a call in a driving voice's own exchange continues that exchange; it ends one that answers or speaks a held answer"},
	"the wake was misheard":   {cannot: "it ends only an exchange whose voice does not drive its turn"},
	"barge-in":                {cannot: "a barge-in cuts a driving voice and keeps its exchange (cutDriverLocked)"},
	"its session ended":       {cannot: "it ends only an exchange speaking a held answer"},
	"the voice did not open":  {cannot: "it ends an exchange before its voice could speak"},
	"turnWaited(reason)":      {cannot: "turnWaited does nothing for a voice that drives its turn"},
	"the voice did not finish": {cannot: "it ends an exchange whose speech the Supervisor waits for itself (finish): a held answer, or the answer of a voice that does not drive its turn"},
	"done":                    {cannot: "it ends an exchange whose speech the Supervisor waits for itself (finish): a held answer, or the answer of a voice that does not drive its turn"},
}

// spokenCount is how many voice_spoken events r saw for sess-1.
func spokenCount(r *rig) int {
	count := 0
	for _, e := range r.events.all() {
		if e == "voice_spoken:sess-1" {
			count++
		}
	}
	return count
}

// TestEveryEndAfterADrivingVoiceSpokeReportsItSpokenOnce fails if an
// exchange whose voice drives its own turn, and whose speech the speaker
// played to its end, ends in any way endLocked is called for without sending
// voice_spoken exactly once, while the voice's own report that its speech
// ended trails by a second, past the end. The ways are read from
// supervisor.go (endReasons): a reason spokenEnds has no entry for, or one
// it lists that is gone, fails the test, so a new end is not missed. Review
// 6004768767 of #143 found two of them that lost the event.
func TestEveryEndAfterADrivingVoiceSpokeReportsItSpokenOnce(t *testing.T) {
	reasons := endReasons(t)
	for _, reason := range reasons {
		if _, ok := spokenEnds[reason]; !ok {
			t.Errorf("endLocked is called with %q, which spokenEnds does not list: say how a driving voice's exchange ends so after it spoke, or why it cannot", reason)
		}
	}
	for reason := range spokenEnds {
		if !slices.Contains(reasons, reason) {
			t.Errorf("spokenEnds lists %q, which supervisor.go no longer ends an exchange with", reason)
		}
	}
	for _, reason := range reasons {
		path, ok := spokenEnds[reason]
		if !ok || path.end == nil {
			continue
		}
		t.Run(reason, func(t *testing.T) {
			if path.limit > 0 {
				was := exchangeMax
				exchangeMax = path.limit
				t.Cleanup(func() { exchangeMax = was })
			}
			c := newFakeConcierge("It ships tomorrow.")
			r := newRigWith(t, c, func(cfg *SupervisorConfig, r *rig) {
				cfg.Voice = func() (Voice, error) { return drivingVoice{r.voice}, nil }
				cfg.IdleWait, cfg.FollowUpWait = time.Minute, time.Minute
				if path.idle > 0 {
					cfg.IdleWait = path.idle
				}
				if path.followUp > 0 {
					cfg.FollowUpWait = path.followUp
				}
			})
			r.heard("Wispr, what is happening?", true, true)
			sess := r.waitSession()
			sess.mu.Lock()
			sess.endLate = time.Second
			sess.mu.Unlock()
			speakAs(t, sess, "The issue is in review since this morning.")
			r.waitSpeaking()
			r.speaker.finish(audio.InputVoice)
			eventually(t, "the follow-up window", func() bool {
				r.s.mu.Lock()
				defer r.s.mu.Unlock()
				return r.s.ex != nil && r.s.ex.followUpTimer != nil
			})
			path.end(t, r, c, sess)
			eventually(t, "the exchange ended", func() bool { _, _, closed := sess.state(); return closed })
			time.Sleep(50 * time.Millisecond)
			if n := spokenCount(r); n != 1 {
				t.Fatalf("voice_spoken %d times, want once; events %v", n, r.events.all())
			}
		})
	}
}

// TestADrivingVoiceThatDidNotFinishIsNotReportedSpoken fails if
// voice_spoken is sent for an exchange whose voice never spoke, or whose
// speech a stop or a barge-in cut, or more than once for a speech whose
// voice reported its end before the exchange ended.
func TestADrivingVoiceThatDidNotFinishIsNotReportedSpoken(t *testing.T) {
	for _, tc := range []struct {
		name  string
		speak func(t *testing.T, r *rig, sess *fakeSession)
		want  int
	}{
		{"it never spoke", func(*testing.T, *rig, *fakeSession) {}, 0},
		{"a stop cut its speech", func(t *testing.T, r *rig, sess *fakeSession) {
			speakAs(t, sess, "The issue is in review since this morning.")
			r.waitSpeaking()
			r.heard("Wispr, stop.", true, true)
		}, 0},
		{"a barge-in cut its speech", func(t *testing.T, r *rig, sess *fakeSession) {
			speakAs(t, sess, "The issue is in review since this morning.")
			r.waitSpeaking()
			r.heard("Wispr, wait, what about the other one?", true, true)
			eventually(t, "the cut", func() bool { interrupted, _, _ := sess.state(); return !interrupted.IsZero() })
		}, 0},
		{"its voice reported the end first", func(t *testing.T, r *rig, sess *fakeSession) {
			speakAs(t, sess, "The issue is in review since this morning.")
			r.waitSpeaking()
			r.speaker.finish(audio.InputVoice)
			eventually(t, "the voice's own report", func() bool {
				r.s.mu.Lock()
				defer r.s.mu.Unlock()
				return r.s.ex != nil && r.s.ex.followUpTimer != nil
			})
		}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newDrivingRig(t, 300*time.Millisecond)
			r.heard("Wispr, what is happening?", true, true)
			sess := r.waitSession()
			tc.speak(t, r, sess)
			eventually(t, "the exchange ended", func() bool { _, _, closed := sess.state(); return closed })
			time.Sleep(50 * time.Millisecond)
			if n := spokenCount(r); n != tc.want {
				t.Fatalf("voice_spoken %d times, want %d; events %v", n, tc.want, r.events.all())
			}
			if !slices.Contains(r.events.all(), "voice_exchange_started:sess-1") {
				t.Fatal("control: no exchange started")
			}
		})
	}
}
