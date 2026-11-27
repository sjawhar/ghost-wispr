// Sol's twenty-second review of #191 at 606c751
// (https://github.com/sjawhar/ghost-wispr/pull/191#pullrequestreview-6082473039):
// Sol's parse-lane probes, kept as regression tests, unchanged. The history
// the Concierge reads is a view of the retained segments, cut by the same one
// parse as the request.

package concierge

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/dispatch"
)

func parseLaneClaim(t *testing.T, r *cutRig) string {
	t.Helper()
	view := r.sess.x.Room()
	if view.Request == "" || !r.sess.x.Claim(view.Turn, view.Revision) {
		t.Fatalf("no claimable request: live=%v turn=%d revision=%d request=%q", view.Live, view.Turn, view.Revision, view.Request)
	}
	return view.Request
}

func parseLaneRig(t *testing.T) *cutRig {
	t.Helper()
	r := newCutRigOpening(t, []string{"Wispr, what about ABC-12?"}, true, answers(8)...)
	q := parseLaneClaim(t, r)
	r.answer(t, "initial", q)
	return r
}

func parseLaneHistory(t *testing.T, r *cutRig) []string {
	t.Helper()
	r.seen.mu.Lock()
	defer r.seen.mu.Unlock()
	if len(r.seen.answered) == 0 {
		t.Fatal("control: no Concierge.Answer exchange captured")
	}
	var out []string
	for _, turn := range r.seen.answered[len(r.seen.answered)-1].Recent {
		out = append(out, turn.Text)
	}
	if len(out) == 0 {
		t.Fatal("control: the answer's history enumerated nothing")
	}
	return out
}

func parseLaneFiller(i int) string {
	return fmt.Sprintf("context passage %03d describes detailed interface behavior throughout a long example without issuing any command", i)
}

func TestParseLaneLongQuoteRequestHistory(t *testing.T) {
	const first = `does ABC-12 show "Please`
	const last = `then retry" in the tooltip?`
	for _, endpoints := range []bool{false, true} {
		for _, command := range []string{"retry", "cancel"} {
			for _, actualCancel := range []bool{false, true} {
				t.Run(fmt.Sprintf("endpoints_%v/%s/actual_cancel_%v", endpoints, command, actualCancel), func(t *testing.T) {
					r := parseLaneRig(t)
					finals := []string{first}
					for i := range 72 {
						finals = append(finals, parseLaneFiller(i))
					}
					finals = append(finals, command)
					for i := 72; i < 144; i++ {
						finals = append(finals, parseLaneFiller(i))
					}
					finals = append(finals, last)
					raw := strings.Join(finals, "\n")
					if rest, taken := TakeBack(raw); taken || rest != raw {
						t.Fatal("control: the complete parser did not recognize the closed quotation")
					}
					for _, final := range finals {
						r.hearNow(final, endpoints)
					}
					want := strings.Join(finals, " ")
					if actualCancel {
						r.hearNow("never mind. is ABC-12 merged?", true)
						want = "is ABC-12 merged?"
					}
					got := r.sess.x.Room().Request
					if got != want {
						t.Errorf("request mismatch: got %d bytes (opener=%v command=%v), want %d bytes", len(got), strings.Contains(got, first), strings.Contains(got, "Please "+command), len(want))
					}
					if got == "" {
						t.Fatal("no request available for the backend history probe")
					}
					q := parseLaneClaim(t, r)
					r.answer(t, "long", q)
					history := parseLaneHistory(t, r)
					missing := 0
					for _, final := range finals {
						has := slices.Contains(history, final)
						if has == actualCancel {
							missing++
							if missing <= 3 {
								t.Errorf("history entry %q retained=%v, want %v", final, has, !actualCancel)
							}
						}
					}
					if missing > 0 {
						t.Errorf("history disagrees with the completed quotation/cut on %d of %d finals", missing, len(finals))
					}
					if actualCancel && !slices.Contains(history, "is ABC-12 merged?") {
						t.Error("history lost the replacement after the actual cancellation")
					}
					t.Logf("finals=%d raw_bytes=%d request_bytes=%d history_entries=%d disagreement=%d", len(finals), len(raw), len(got), len(history), missing)
				})
			}
		}
	}
}

func TestParseLaneClaimedQuotePrefix(t *testing.T) {
	for _, earlyClaim := range []bool{false, true} {
		t.Run(fmt.Sprintf("claimed_prefix_%v", earlyClaim), func(t *testing.T) {
			r := parseLaneRig(t)
			prefix := []string{`does ABC-12 show "Please`}
			for i := range 96 {
				prefix = append(prefix, parseLaneFiller(i))
			}
			for _, final := range prefix {
				r.hearNow(final, true)
			}
			if earlyClaim {
				q := parseLaneClaim(t, r)
				r.answer(t, "prefix", q)
			}
			suffix := []string{"cancel", `then retry" in the tooltip?`}
			for _, final := range suffix {
				r.hearNow(final, true)
			}
			want := strings.Join(suffix, " ")
			if !earlyClaim {
				want = strings.Join(append(prefix, suffix...), " ")
			}
			got := r.sess.x.Room().Request
			if got != want {
				t.Errorf("quoted cancel after a committed prefix: got %q, want %d bytes beginning with %q", got, len(want), want[:min(32, len(want))])
			}
			if got != "" {
				q := parseLaneClaim(t, r)
				r.answer(t, "suffix", q)
				history := parseLaneHistory(t, r)
				if !slices.Contains(history, prefix[0]) || !slices.Contains(history, "cancel") {
					t.Errorf("completed quotation history lost opener=%v or quoted cancel=%v", slices.Contains(history, prefix[0]), slices.Contains(history, "cancel"))
				}
				t.Logf("prefix_finals=%d prefix_bytes=%d request_bytes=%d history_entries=%d", len(prefix), len(strings.Join(prefix, "\n")), len(got), len(history))
			}
		})
	}
}

func TestParseLaneRetiredHistorySurvivesTakeback(t *testing.T) {
	const prior = "when will ABC-12 ship?"
	for _, retirement := range []string{"claimed", "answered"} {
		for _, endpoint := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/endpoint_%v", retirement, endpoint), func(t *testing.T) {
				r := parseLaneRig(t)
				r.hearNow(prior, endpoint)
				if retirement == "claimed" {
					q := parseLaneClaim(t, r)
					r.answer(t, "prior", q)
				} else {
					if r.sess.x.Answered == nil {
						t.Fatal("Answered hook unavailable")
					}
					r.sess.x.Answered(time.Now())
				}
				r.hearNow("never mind. is ABC-12 merged?", true)
				got := r.sess.x.Room().Request
				if got != "is ABC-12 merged?" {
					t.Fatalf("request=%q, want only the replacement", got)
				}
				q := parseLaneClaim(t, r)
				r.answer(t, "replacement", q)
				history := parseLaneHistory(t, r)
				if !slices.Contains(history, prior) {
					t.Errorf("%s question left backend history after takeback (endpoint=%v): %q", retirement, endpoint, history)
				}
				if !slices.Contains(history, "is ABC-12 merged?") {
					t.Error("replacement missing from backend history")
				}
			})
		}
	}
}

type parseLaneDelayedSearch struct {
	Records
	release chan struct{}
	done    chan error
}

func (d *parseLaneDelayedSearch) Search(ctx context.Context, query string, limit int) ([]dispatch.SearchHit, error) {
	<-d.release
	hits, err := d.Records.Search(ctx, query, limit)
	d.done <- err
	return hits, err
}

func TestParseLaneSearchEntryVersusOutbound(t *testing.T) {
	t.Run("live_control_then_cancelled_at_entry", func(t *testing.T) {
		var calls atomic.Int64
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			_, _ = fmt.Fprint(w, `{"results":[]}`)
		}))
		defer server.Close()
		client, err := dispatch.NewClient(server.URL, "loopback-not-a-secret", server.Client())
		if err != nil {
			t.Fatal(err)
		}
		fake := &fakeRecords{}
		if _, err := client.Search(t.Context(), "live control", 10); err != nil {
			t.Fatal(err)
		}
		_, _ = fake.Search(t.Context(), "live control", 10)
		if calls.Load() != 1 || fake.sentSearches() != 1 {
			t.Fatal("live control failed to observe outbound request and fake count")
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := client.Search(ctx, "cancelled at entry", 10); err == nil {
			t.Error("pre-cancelled real call returned no error")
		}
		_, _ = fake.Search(ctx, "cancelled at entry", 10)
		if calls.Load() != 1 || fake.sentSearches() != 1 || len(fake.searches()) != 2 {
			t.Errorf("outbound=%d sentSearches=%d entries=%d, want 1/1/2", calls.Load(), fake.sentSearches(), len(fake.searches()))
		}
		t.Logf("live then precancelled: outbound=%d fake_sent=%d fake_entries=%d", calls.Load(), fake.sentSearches(), len(fake.searches()))
	})
	t.Run("keyword_win_before_full_entry", func(t *testing.T) {
		var full, keywords atomic.Int64
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/search/results" {
				keywords.Add(1)
			} else {
				full.Add(1)
			}
			_, _ = fmt.Fprint(w, `{"results":[{"kind":"issue","issue":{"key":"ABC-12","title":"Realtime voice","status":"in_progress"},"rank":0.9}]}`)
		}))
		defer server.Close()
		client, err := dispatch.NewClient(server.URL, "loopback-not-a-secret", server.Client())
		if err != nil {
			t.Fatal(err)
		}
		fake := &fakeRecords{kwHits: []dispatch.SearchHit{{Kind: "issue", IssueKey: "ABC-12", Title: "Realtime voice", Score: 0.9}}}
		r := newRig(t)
		for _, records := range []Records{fake, client} {
			delayed := &parseLaneDelayedSearch{Records: records, release: make(chan struct{}), done: make(chan error, 1)}
			if _, err := r.concierge.searchBoth(t.Context(), delayed, "realtime voice", "realtime voice", nil); err != nil {
				t.Fatal(err)
			}
			close(delayed.release)
			select {
			case err := <-delayed.done:
				t.Logf("the delayed full Search returned %v", err)
			case <-time.After(time.Second):
				t.Fatal("delayed full Search did not return")
			}
		}
		if len(fake.searches()) != 1 || fake.sentSearches() != 0 || full.Load() != 0 || keywords.Load() != 1 {
			t.Errorf("keyword winner: fake_entries=%d fake_sent=%d real_full=%d real_keywords=%d, want 1/0/0/1", len(fake.searches()), fake.sentSearches(), full.Load(), keywords.Load())
		}
		t.Logf("delayed full after keyword win: fake_entries=%d fake_sent=%d real_full=%d real_keywords=%d", len(fake.searches()), fake.sentSearches(), full.Load(), keywords.Load())
	})
	t.Run("cancelled_after_real_outbound_is_counted", func(t *testing.T) {
		started := make(chan struct{})
		var calls atomic.Int64
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			close(started)
			<-r.Context().Done()
		}))
		defer server.Close()
		client, err := dispatch.NewClient(server.URL, "loopback-not-a-secret", server.Client())
		if err != nil {
			t.Fatal(err)
		}
		fake := &fakeRecords{searchDelay: time.Hour}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		realDone, fakeDone := make(chan error, 1), make(chan error, 1)
		go func() { _, err := client.Search(ctx, "in flight", 10); realDone <- err }()
		go func() { _, err := fake.Search(ctx, "in flight", 10); fakeDone <- err }()
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("control: real outbound never reached loopback server")
		}
		eventually(t, "fake live Search counted", func() bool { return fake.sentSearches() == 1 })
		cancel()
		for _, done := range []chan error{realDone, fakeDone} {
			select {
			case err := <-done:
				if err == nil {
					t.Error("cancelled in-flight Search returned no error")
				}
			case <-time.After(time.Second):
				t.Fatal("cancelled Search did not stop")
			}
		}
		if calls.Load() != 1 || fake.sentSearches() != 1 {
			t.Errorf("outbound=%d sentSearches=%d, want 1/1", calls.Load(), fake.sentSearches())
		}
		t.Logf("in-flight cancellation: real_outbound=%d fake_sent=%d", calls.Load(), fake.sentSearches())
	})
}
