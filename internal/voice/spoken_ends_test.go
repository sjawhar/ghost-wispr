package voice

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strconv"
	"sync"
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
	// afterReport, when set, says why the end can come only after the
	// speaker's report that the speech ended: with that report held, the
	// test checks the exchange stays open, then releases the report and
	// ends it. Unset, the end comes before the report too, and the test
	// ends the exchange with the report held.
	afterReport string
	// idle and followUp are the rig's IdleWait and FollowUpWait; 0 is a
	// minute, so neither ends the exchange first. limit is exchangeMax; 0
	// leaves it.
	idle, followUp, limit time.Duration
	// meeting ends the exchange's meeting (SupervisorConfig.Meeting) as the
	// end, before end runs.
	meeting bool
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
	"idle": {end: func(*testing.T, *rig, *fakeConcierge, *fakeSession) {}, idle: 300 * time.Millisecond,
		afterReport: "IdleWait ends an exchange only while the voice is silent (idleWaited), which it is once the speaker reports the speech's end"},
	"no follow-up": {end: func(*testing.T, *rig, *fakeConcierge, *fakeSession) {}, followUp: 300 * time.Millisecond,
		afterReport: "the follow-up window opens on the speaker's report (driverSpeechLocked)"},
	"it reached its limit of %v": {end: func(*testing.T, *rig, *fakeConcierge, *fakeSession) {},
		limit: time.Second},
	"Wispr, who has a hand up": {end: func(t *testing.T, r *rig, _ *fakeConcierge, _ *fakeSession) {
		r.heard("Wispr, who has a hand up?", true, true)
	}},
	"its meeting ended": {end: func(*testing.T, *rig, *fakeConcierge, *fakeSession) {}, meeting: true},
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
	}, afterReport: "side talk ends an exchange only in the follow-up window, which opens on the speaker's report"},
	"a holder's answer takes the floor": {end: func(t *testing.T, r *rig, c *fakeConcierge, _ *fakeSession) {
		c.answers <- PendingAnswer{TaskID: "holder", SessionID: "sess-1", IssueKey: "ABC-12", Text: "It ships tomorrow.", AskedAt: time.Now()}
		if got := waitSaid(t, r.waitSession()); got != "It ships tomorrow." {
			t.Fatalf("control: the holder's answer said %q", got)
		}
	}, afterReport: "a holder's answer takes the floor only while the voice is silent (offerAnswers)"},

	"Wispr, go ahead":          {cannot: "it ends only an exchange whose voice does not drive its turn"},
	"its membership ended":     {cannot: "it ends only an exchange speaking a hand's held answer (handEndedLocked)"},
	"a new call":               {cannot: "a call in a driving voice's own exchange continues that exchange; it ends one that answers or speaks a held answer"},
	"the wake was misheard":    {cannot: "it ends only an exchange whose voice does not drive its turn"},
	"barge-in":                 {cannot: "a barge-in cuts a driving voice and keeps its exchange (cutDriverLocked)"},
	"its session ended":        {cannot: "it ends only an exchange speaking a held answer"},
	"the voice did not open":   {cannot: "it ends an exchange before its voice could speak"},
	"turnWaited(reason)":       {cannot: "turnWaited does nothing for a voice that drives its turn"},
	"the voice did not finish": {cannot: "it ends an exchange whose speech the Supervisor waits for itself (finish): a held answer, or the answer of a voice that does not drive its turn"},
	"done":                     {cannot: "it ends an exchange whose speech the Supervisor waits for itself (finish): a held answer, or the answer of a voice that does not drive its turn"},
}

// heldReports holds the speaker's report that a speech on the voice input
// ended by itself (not cancelled) until release, as the speaker's callbacks,
// delivered on a goroutine of their own, can come after the exchange has
// moved on. Every other report passes at once.
type heldReports struct {
	*fakeSpeaker
	held, released, delivered chan struct{}
	release                   func()
	// first is the first report a speech ended by itself: the one held.
	first          sync.Once
	heldOnce, done sync.Once
}

// holdReports makes r's speaker hold its next report that a voice speech
// ended by itself, in a newRigWith configuration.
func holdReports(t *testing.T, cfg *SupervisorConfig, r *rig) *heldReports {
	h := &heldReports{fakeSpeaker: r.speaker, held: make(chan struct{}), released: make(chan struct{}), delivered: make(chan struct{})}
	h.release = sync.OnceFunc(func() { close(h.released) })
	t.Cleanup(h.release)
	cfg.Speaker = h
	return h
}

func (h *heldReports) OnSpeech(fn func(audio.SpeechEvent)) func() {
	return h.fakeSpeaker.OnSpeech(func(e audio.SpeechEvent) {
		if e.Input == audio.InputVoice && !e.Started && !e.Cancelled {
			isFirst := false
			h.first.Do(func() { isFirst = true })
			if isFirst {
				h.heldOnce.Do(func() { close(h.held) })
				<-h.released
				fn(e)
				h.done.Do(func() { close(h.delivered) })
				return
			}
		}
		fn(e)
	})
}

// waitFor fails t unless ch closes within testWait.
func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(testWait):
		t.Fatalf("timed out waiting for %s", what)
	}
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
// ended trails by a second, past the end. Each way runs in two orders: the
// speaker's report of the speech's end comes before the end, or is held past
// it, as the speaker's callbacks, on a goroutine of their own, can be. An end
// that can come only after that report (afterReport) must keep the exchange
// open while it is held. The ways are read from supervisor.go (endReasons):
// a reason spokenEnds has no entry for, or one it lists that is gone, fails
// the test, so a new end is not missed. Reviews 6004768767 and 6005017841 of
// #143 found endings, in each order, that lost the event.
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
		for _, late := range []bool{false, true} {
			order := "the speaker reports the speech's end first"
			if late {
				order = "the speaker's report is held past the end"
			}
			t.Run(reason+"/"+order, func(t *testing.T) {
				if path.limit > 0 {
					was := exchangeMax
					exchangeMax = path.limit
					t.Cleanup(func() { exchangeMax = was })
				}
				c := newFakeConcierge("It ships tomorrow.")
				meeting, endMeeting := context.WithCancel(context.Background())
				t.Cleanup(endMeeting)
				var reports *heldReports
				r := newRigWith(t, c, func(cfg *SupervisorConfig, r *rig) {
					reports = holdReports(t, cfg, r)
					cfg.Voice = func() (Voice, error) { return drivingVoice{r.voice}, nil }
					cfg.Meeting = func(string) context.Context { return meeting }
					cfg.IdleWait, cfg.FollowUpWait = time.Minute, time.Minute
					if path.idle > 0 {
						cfg.IdleWait = path.idle
					}
					if path.followUp > 0 {
						cfg.FollowUpWait = path.followUp
					}
				})
				var sess *fakeSession
				end := func() {
					if path.meeting {
						endMeeting()
					}
					path.end(t, r, c, sess)
				}
				r.heard("Wispr, what is happening?", true, true)
				sess = r.waitSession()
				sess.mu.Lock()
				sess.endLate = time.Second
				sess.mu.Unlock()
				speakAs(t, sess, "The issue is in review since this morning.")
				r.waitSpeaking()
				r.speaker.finish(audio.InputVoice)
				waitFor(t, reports.held, "the speaker's report of the speech's end")
				closed := func() bool { _, _, closed := sess.state(); return closed }
				switch {
				case !late:
					reports.release()
					waitFor(t, reports.delivered, "the speaker's report delivered")
					end()
				case path.afterReport != "":
					time.Sleep(3 * max(path.idle, path.followUp, 300*time.Millisecond))
					if closed() {
						t.Fatalf("the exchange ended with the speaker's report held, though %s", path.afterReport)
					}
					reports.release()
					waitFor(t, reports.delivered, "the speaker's report delivered")
					end()
				default:
					end()
					eventually(t, "the exchange ended with the speaker's report held", closed)
					reports.release()
					waitFor(t, reports.delivered, "the speaker's report delivered")
				}
				eventually(t, "the exchange ended", closed)
				time.Sleep(50 * time.Millisecond)
				if n := spokenCount(r); n != 1 {
					t.Fatalf("voice_spoken %d times, want once; events %v", n, r.events.all())
				}
			})
		}
	}
}

// TestADrivingVoiceThatDidNotFinishIsNotReportedSpoken fails if
// voice_spoken is sent for an exchange whose voice never spoke, or whose
// speech a stop or a barge-in cut, or more than once for a speech whose
// voice reported its end before the exchange ended; or if it is not sent for
// a speech that played to its end before a barge-in came, with the speaker's
// report of that end held past the barge-in (review 6005017841 of #143).
func TestADrivingVoiceThatDidNotFinishIsNotReportedSpoken(t *testing.T) {
	for _, tc := range []struct {
		name  string
		speak func(t *testing.T, r *rig, sess *fakeSession, reports *heldReports)
		want  int
	}{
		{"it never spoke", func(*testing.T, *rig, *fakeSession, *heldReports) {}, 0},
		{"a stop cut its speech", func(t *testing.T, r *rig, sess *fakeSession, _ *heldReports) {
			speakAs(t, sess, "The issue is in review since this morning.")
			r.waitSpeaking()
			r.heard("Wispr, stop.", true, true)
		}, 0},
		{"a barge-in cut its speech", func(t *testing.T, r *rig, sess *fakeSession, _ *heldReports) {
			speakAs(t, sess, "The issue is in review since this morning.")
			r.waitSpeaking()
			r.heard("Wispr, wait, what about the other one?", true, true)
			eventually(t, "the cut", func() bool { interrupted, _, _ := sess.state(); return !interrupted.IsZero() })
		}, 0},
		{"a barge-in came after its speech played", func(t *testing.T, r *rig, sess *fakeSession, reports *heldReports) {
			speakAs(t, sess, "The issue is in review since this morning.")
			r.waitSpeaking()
			r.speaker.finish(audio.InputVoice)
			waitFor(t, reports.held, "the speaker's report of the speech's end")
			r.heard("Wait, what about the other issue?", true, true)
			eventually(t, "the barge-in", func() bool { interrupted, _, _ := sess.state(); return !interrupted.IsZero() })
			reports.release()
			waitFor(t, reports.delivered, "the speaker's report delivered")
		}, 1},
		{"its voice reported the end first", func(t *testing.T, r *rig, sess *fakeSession, reports *heldReports) {
			speakAs(t, sess, "The issue is in review since this morning.")
			r.waitSpeaking()
			reports.release()
			r.speaker.finish(audio.InputVoice)
			waitFor(t, reports.delivered, "the speaker's report delivered")
		}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reports *heldReports
			r := newRigWith(t, newFakeConcierge(), func(cfg *SupervisorConfig, r *rig) {
				reports = holdReports(t, cfg, r)
				cfg.Voice = func() (Voice, error) { return drivingVoice{r.voice}, nil }
				cfg.IdleWait, cfg.FollowUpWait = 300*time.Millisecond, 300*time.Millisecond
			})
			r.heard("Wispr, what is happening?", true, true)
			sess := r.waitSession()
			tc.speak(t, r, sess, reports)
			r.s.Stop("the test ends the exchange")
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

// TestASpeechFromBeforeAnExchangeIsNotItsSpoken fails if a speech that
// played to its end outside any exchange (POST /api/tts/speak), whose
// speaker report is held until a new exchange holds the floor, marks that
// exchange, whose voice never spoke, as spoken: a speech belongs to the
// exchange that held the floor when it started (speechEx). From review
// 6005017841 of #143.
func TestASpeechFromBeforeAnExchangeIsNotItsSpoken(t *testing.T) {
	var reports *heldReports
	r := newRigWith(t, newFakeConcierge(), func(cfg *SupervisorConfig, r *rig) {
		reports = holdReports(t, cfg, r)
		cfg.Voice = func() (Voice, error) { return drivingVoice{r.voice}, nil }
		cfg.IdleWait, cfg.FollowUpWait = time.Minute, time.Minute
	})
	pb := r.speaker.Begin(audio.InputVoice)
	if err := pb.Write(t.Context(), make([]byte, 960), audio.AudioFormat{SampleRate: 24000, Channels: 1, Encoding: audio.EncodingPCM16}); err != nil {
		t.Fatal(err)
	}
	r.waitSpeaking()
	r.speaker.finish(audio.InputVoice)
	waitFor(t, reports.held, "the earlier speech's report")
	r.heard("Wispr, what is happening?", true, true)
	_ = r.waitSession()
	r.waitEvent("voice_exchange_started:sess-1")
	reports.release()
	waitFor(t, reports.delivered, "the earlier speech's report delivered")
	r.s.Stop("the test ends the exchange")
	time.Sleep(50 * time.Millisecond)
	if n := spokenCount(r); n != 0 {
		t.Fatalf("voice_spoken %d times for a new exchange whose voice never spoke; events %v", n, r.events.all())
	}
}
