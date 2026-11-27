package concierge

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/dispatch"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

type pendingSendSlowRecords struct{ Records }

func (r pendingSendSlowRecords) Issue(ctx context.Context, key string) (*dispatch.Issue, error) {
	select {
	case <-time.After(2500 * time.Millisecond):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return r.Records.Issue(ctx, key)
}

func (r pendingSendSlowRecords) DocumentText(ctx context.Context, _, _ string) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}

func (r pendingSendSlowRecords) Events(ctx context.Context, key string, before int64, limit int) ([]dispatch.Event, error) {
	select {
	case <-time.After(3 * time.Second):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return r.Records.Events(ctx, key, before, limit)
}

// TestPendingSendMatrix keeps the fast answer inside its bound while Envoy's
// send runs beside it. The line about the ask and the named key do not depend
// on which completes first. The slow send is held until Answer has returned,
// so it outlasts the answer however slowly the machine runs, and the bound is
// the production 8 s, so a send that begins late still begins inside it. On
// a loaded sami-devbox the question reached Envoy up to 1.6 s after the turn:
// a send of 3 s then ended more than 2 s after a 2 s bound, and a failed send
// came after it. Each row has a rig of its own, so the rows run in parallel.
func TestPendingSendMatrix(t *testing.T) {
	t.Parallel()
	for _, send := range []struct {
		name  string
		delay time.Duration
		// held: the send returns only once Answer has returned.
		held bool
		fail bool
	}{
		{name: "send_in_10_ms", delay: 10 * time.Millisecond},
		{name: "send_in_400_ms", delay: 400 * time.Millisecond},
		{name: "send_after_the_answer", held: true},
		{name: "send_failure", fail: true},
	} {
		for _, keyless := range []bool{false, true} {
			name := send.name + "/keyed_first_fact"
			if keyless {
				name = send.name + "/keyless_first_then_valid_second"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				r := newRig(t)
				heldIssue(r, "session", false)
				r.concierge.knownBound = 20 * time.Second
				if r.concierge.fastBound != 8*time.Second {
					t.Fatal("the matrix did not use the production answer bound")
				}
				answered := make(chan struct{})
				release := sync.OnceFunc(func() { close(answered) })
				// A test that fails before Answer returns releases the send,
				// before the rig's cleanup waits for it.
				t.Cleanup(release)
				finished := make(chan struct{})
				r.agents.onSend = func(ctx context.Context, _ string) error {
					defer close(finished)
					switch {
					case send.fail:
						return errors.New("send failed")
					case send.held:
						select {
						case <-answered:
							return nil
						case <-ctx.Done():
							return ctx.Err()
						}
					}
					select {
					case <-time.After(send.delay):
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				first := "ABC-12 is in progress, updated 2 hours ago."
				if keyless {
					first = "It is in progress, updated 2 hours ago."
				}
				r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text(first))}, {chunk(text("ABC-12 is in progress, updated 2 hours ago."))}}
				var spoken []said
				start := time.Now()
				err := r.concierge.Answer(context.Background(), &r.exchange, "ask the agent on ABC-12 when it will be ready?", recorder(&spoken))
				elapsed := time.Since(start)
				release()
				if err != nil && !send.fail {
					t.Fatalf("Answer = %v", err)
				}
				// The send ends by its own bound at the latest; this waits for
				// it, and fails only if it never began.
				select {
				case <-finished:
				case <-time.After(sendTimeout):
					t.Fatalf("the send did not finish within %v", sendTimeout)
				}
				text := make([]string, len(spoken))
				for i := range spoken {
					text[i] = spoken[i].text
				}
				joined := strings.Join(text, " ")
				if !strings.Contains(joined, "ABC-12") {
					t.Fatalf("spoken = %q, want the key", joined)
				}
				want := "I asked the agent on ABC-12."
				if send.fail {
					want = "I could not ask the agent on ABC-12:"
				}
				if !strings.Contains(joined, want) {
					t.Fatalf("spoken = %q, want %q", joined, want)
				}
				if elapsed > r.concierge.fastBound+time.Second {
					t.Fatalf("Answer took %v, want at most the %v bound plus speech queue", elapsed, r.concierge.fastBound)
				}
			})
		}
	}
}

// TestPendingSendRetryProviderFailureReturnsError: a provider failure on the
// second R8 attempt stays an error even after the first fact was spoken. The
// send is still pending when the answer's bound ends; the answer waits for it
// that long, so a bound of 2 s, not the 8 s one, ends the test sooner.
func TestPendingSendRetryProviderFailureReturnsError(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	heldIssue(r, "session", false)
	r.concierge.knownBound = 20 * time.Second
	r.concierge.fastBound = 2 * time.Second
	release := make(chan struct{})
	r.agents.onSend = func(context.Context, string) error {
		<-release
		return nil
	}
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("ABC-12 is in progress. The change waits for Alex."))}}
	failure := errors.New("provider failed")
	r.model.errs = []error{nil, failure}
	var spoken []said
	err := r.concierge.Answer(context.Background(), &r.exchange, "ask the agent on ABC-12 when it will be ready?", recorder(&spoken))
	close(release)
	if err == nil {
		t.Fatal("the retry provider failed but Answer returned nil")
	}
	// The error's text is withheld (providererr): it can quote the prompt.
	if !errors.Is(err, voice.ErrUnreachable) && !errors.Is(err, failure) {
		t.Fatalf("Answer = %v, want the provider failure", err)
	}
}

// TestPendingSendMatrixSlowRecord: a slow record read consumes the whole fast
// answer bound. The answer returns then, without waiting for Envoy's send.
// The slow send is held until Answer has returned, so it outlasts the answer
// however slowly the machine runs, as TestPendingSendMatrix's is. Each row
// has a rig of its own, so the rows run in parallel.
func TestPendingSendMatrixSlowRecord(t *testing.T) {
	t.Parallel()
	for _, send := range []struct {
		name string
		// held: the send returns only once Answer has returned.
		held bool
	}{
		{name: "send_in_10_ms"},
		{name: "send_after_the_answer", held: true},
	} {
		t.Run(send.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			heldIssue(r, "session", false)
			issue := r.records.issues["ABC-12"]
			issue.PrimaryArtifactID = "spec-1"
			issue.Artifacts = append(issue.Artifacts, struct {
				ID   string `json:"id"`
				Slug string `json:"slug"`
			}{ID: "spec-1", Slug: "spec"})
			r.concierge.records = pendingSendSlowRecords{Records: r.records}
			if r.concierge.fastBound != 8*time.Second || r.concierge.callBound != 3*time.Second {
				t.Fatal("the matrix did not use production read and answer bounds")
			}
			answered := make(chan struct{})
			release := sync.OnceFunc(func() { close(answered) })
			// A test that fails before Answer returns releases the send,
			// before the rig's cleanup waits for it.
			t.Cleanup(release)
			finished := make(chan struct{})
			r.agents.onSend = func(ctx context.Context, _ string) error {
				defer close(finished)
				if send.held {
					select {
					case <-answered:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				select {
				case <-time.After(10 * time.Millisecond):
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			var spoken []said
			start := time.Now()
			err := r.concierge.Answer(context.Background(), &r.exchange, "ask the agent on ABC-12 when it will be ready?", recorder(&spoken))
			elapsed := time.Since(start)
			release()
			// The send ends by its own bound at the latest; this waits for
			// it, and fails only if it never began.
			select {
			case <-finished:
			case <-time.After(sendTimeout):
				t.Fatalf("the send did not finish within %v", sendTimeout)
			}
			r.agents.mu.Lock()
			sends := len(r.agents.sent)
			r.agents.mu.Unlock()
			if err != nil || sends != 1 || len(r.model.requests) != 0 {
				t.Fatalf("Answer = %v, sends = %d, model calls = %d; want the slow-record early ask", err, sends, len(r.model.requests))
			}
			if elapsed > r.concierge.fastBound+150*time.Millisecond {
				t.Fatalf("Answer waited for the send: %v > %v", elapsed, r.concierge.fastBound)
			}
		})
	}
}
