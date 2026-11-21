package joined

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/envoy"
)

// heldNotices is the unit's Envoy session with each voice-stopped notice
// held: per mode, until its context ends ("ctx"), or until release closes,
// whatever its context does ("release"). With mode "" the notices go at once.
// entered counts the notices under way.
type heldNotices struct {
	*fakeAgents
	mode    string
	release chan struct{}

	mu      sync.Mutex
	entered map[string]bool
	errs    map[string]error
}

func (h *heldNotices) Send(ctx context.Context, request *envoy.SendRequest) (envoy.SendResult, error) {
	if request.Message != voiceStoppedText || h.mode == "" {
		return h.fakeAgents.Send(ctx, request)
	}
	h.mu.Lock()
	h.entered[request.Target] = true
	h.mu.Unlock()
	var err error
	switch h.mode {
	case "ctx":
		<-ctx.Done()
		err = ctx.Err()
	case "release":
		<-h.release
		err = fmt.Errorf("released")
	}
	h.mu.Lock()
	h.errs[request.Target] = err
	h.mu.Unlock()
	return envoy.SendResult{}, err
}

func (h *heldNotices) underWay() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Sorted(func(yield func(string) bool) {
		for agent := range h.entered {
			if !yield(agent) {
				return
			}
		}
	})
}

// logLines is a Config.Logf that keeps what it is given.
type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logLines) with(part string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, line := range l.lines {
		if strings.Contains(line, part) {
			out = append(out, line)
		}
	}
	return out
}

// TestTheVoiceStoppedNoticesGoTogetherWithinTheStopsDeadline fails if an
// opening's Close, its context's deadline 100 ms off, waits past that
// deadline for the notices that tell each of three joined agents the voice
// stopped: when the notices wait for their contexts, and so for the
// deadline, it returns at once after it; when they ignore their contexts, it
// still returns at the deadline, having sent all three at once, and logs the
// agents not told. Before, the notices went one at a time under the roster's
// own context, ten seconds each, past the shutdown's deadline. The control's
// notices go at once, and nothing is logged. From review 6006723807 of #138.
func TestTheVoiceStoppedNoticesGoTogetherWithinTheStopsDeadline(t *testing.T) {
	agents := []string{"agent-1", "agent-2", "agent-3"}
	for _, mode := range []string{"", "ctx", "release"} {
		name := map[string]string{"": "control: the notices go at once", "ctx": "the notices wait for their contexts", "release": "the notices ignore their contexts"}[mode]
		t.Run(name, func(t *testing.T) {
			logs := &logLines{}
			// Not t.Logf: a held notice's send ends, and logs, after the
			// test returns.
			r := newRig(t, func(cfg *Config) { cfg.Logf = logs.logf })
			held := &heldNotices{fakeAgents: r.agents, mode: mode, release: make(chan struct{}), entered: map[string]bool{}, errs: map[string]error{}}
			defer close(held.release)
			r.open(held)
			for i, agent := range agents {
				r.join(t, agent, []string{"ABC-12", "XYZ-9", "INFRA-1027"}[i])
			}
			if got := r.roster.Joined(); len(got) != len(agents) {
				t.Fatalf("control: %d agents joined, want %d", len(got), len(agents))
			}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			closed := make(chan struct{})
			began := time.Now()
			go func() {
				defer close(closed)
				r.opening.Close(ctx)
			}()
			select {
			case <-closed:
			case <-time.After(2 * time.Second):
				t.Fatalf("Close waited past its deadline for the voice-stopped notices; under way: %v", held.underWay())
			}
			took := time.Since(began)
			if got := r.roster.Joined(); len(got) != 0 {
				t.Errorf("joined after the close: %+v, want none", got)
			}
			untold := logs.with("before these agents were told the voice stopped")
			switch mode {
			case "":
				for _, agent := range agents {
					if got := r.agents.to(agent); len(got) == 0 || got[len(got)-1].Message != voiceStoppedText {
						t.Errorf("control: %s was not told the voice stopped: %+v", agent, got)
					}
				}
				if len(untold) != 0 {
					t.Errorf("control: logged agents not told: %q", untold)
				}
				if took > time.Second {
					t.Errorf("control: Close took %v", took)
				}
			default:
				if got := held.underWay(); !slices.Equal(got, agents) {
					t.Errorf("notices under way at the deadline: %v, want all of %v at once", got, agents)
				}
				if len(untold) != 1 || !strings.Contains(untold[0], strings.Join(agents, ", ")) {
					t.Errorf("logged agents not told: %q, want one line naming %v", untold, agents)
				}
			}
		})
	}
}
