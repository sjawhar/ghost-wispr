// Sol's twenty-second review of #191 at 606c751
// (https://github.com/sjawhar/ghost-wispr/pull/191#pullrequestreview-6082473039):
// Sol's probes through the real Supervisor, Concierge and GPT-Live client,
// kept as regression tests, unchanged. Both fail at 606c751.

package concierge

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/voice"
	"github.com/sjawhar/ghost-wispr/internal/voice/gptlive"
)

func TestSol191R22ClarificationKeyReply(t *testing.T) {
	for _, reply := range []string{"ABC-12", "what is the status of ABC-12?"} {
		t.Run(reply, func(t *testing.T) {
			const question = "what is happening with the recorder?"
			r, v, w := openVoiceBOn(t, question)
			w.send(map[string]any{"type": "session.output_transcript.delta", "delta": "What is its key?"})
			vendorSpeaks(t, w, r)
			r.hearNow(reply, true)
			sendDelegation(w, "reply", 1000)
			call := within(t, v.calls, "key reply call")
			var a voice.AnswerArgs
			if err := json.Unmarshal(call.Args, &a); err != nil {
				t.Fatal(err)
			}
			if a.Question != reply {
				t.Errorf("reply tool question=%q, want %q alone", a.Question, reply)
			}
			if !strings.Contains(a.Transcript, question) {
				t.Errorf("question missing from tool history: %s", a.Transcript)
			}
			result := within(t, v.results, "key reply result")
			var answer voice.AnswerResult
			_ = json.Unmarshal(result, &answer)
			if answer.IssueKey != "ABC-12" {
				t.Errorf("key reply selected %q, result %s", answer.IssueKey, result)
			}
			prompt := lastPrompt(r.rig)
			if prompt == "" {
				t.Fatalf("model not called: result=%s", result)
			}
			// The explicit question the model is asked, not only words
			// somewhere in its prompt, which the history satisfies anyway
			// (Sol's twenty-third review of #191): a key said alone answers
			// the question GPT-Live's own question followed; a question is
			// asked as itself.
			want := question
			if reply != "ABC-12" {
				want = reply
			}
			if got := modelQuestion(r); got != want {
				t.Errorf("the model was asked %q, want %q", got, want)
			}
		})
	}
}

type sol22LogGate struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (*sol22LogGate) Enabled(context.Context, slog.Level) bool { return true }

//nolint:gocritic // slog.Handler's Handle takes the record by value.
func (g *sol22LogGate) Handle(_ context.Context, r slog.Record) error {
	if r.Message == "gptlive say" {
		g.once.Do(func() { close(g.entered); <-g.release })
	}
	return nil
}
func (g *sol22LogGate) WithAttrs([]slog.Attr) slog.Handler { return g }
func (g *sol22LogGate) WithGroup(string) slog.Handler      { return g }

type sol22CapturedVoice struct {
	inner   voice.Voice
	mu      sync.Mutex
	session voice.Session
	room    func() voice.RoomView
}

func (*sol22CapturedVoice) DrivesTurn() bool { return true }
func (v *sol22CapturedVoice) Open(ctx context.Context, x *voice.Exchange) (voice.Session, error) {
	s, err := v.inner.Open(ctx, x)
	v.mu.Lock()
	v.session = s
	v.room = x.Room
	v.mu.Unlock()
	return s, err
}
func (v *sol22CapturedVoice) get() (voice.Session, func() voice.RoomView) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.session, v.room
}

// A synchronous logger is an existing scheduling seam after caused() and
// before appendAll. No runtime method is replaced by this probe.
func TestSol191R22HeldAnswerConcurrentWithOwnSpeech(t *testing.T) {
	const first = "what time is it?"
	const next = "is ABC-12 merged?"
	url, conns := loopbackVendor(t, false)
	g := &sol22LogGate{entered: make(chan struct{}), release: make(chan struct{})}
	live, err := gptlive.New(gptlive.Config{URL: url, Token: func(context.Context) (string, error) { return "loopback-not-a-secret", nil }, Logger: slog.New(g)})
	if err != nil {
		t.Fatal(err)
	}
	cap := &sol22CapturedVoice{inner: live}
	v := watch(cap)
	r := openingRig(t, v)
	heldIssue(r.rig, "session", false)
	r.model.turns = answers(8)
	r.hearNow("Wispr, "+first, true)
	w := within(t, conns, "vendor")
	w.next(t, "session.start")
	within(t, v.opened, "Open")
	s, room := cap.get()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	var once sync.Once
	defer once.Do(func() { close(g.release) })
	go func() { done <- s.Say(ctx, "The holder's late answer is that ABC-12 is in progress.", voice.Answer) }()
	within(t, g.entered, "held Say before its frame is sent")
	vendorSpeaks(t, w, r) // no delegation and no client cause was sent
	if got := room().Request; got != "" {
		t.Errorf("self-answered question survives before held commentary is sent: %q", got)
	}
	r.hearNow(next, true)
	once.Do(func() { close(g.release) })
	w.next(t, "session.commentary.append")
	vendorSpeaks(t, w, r) // caused by the held commentary; must not retire next
	if got := room().Request; got != next {
		t.Errorf("held answer retired the real next question: request=%q, want %q", got, next)
	}
	cancel()
	select {
	case err := <-done:
		t.Logf("Say ended: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("Say did not end")
	}
}
