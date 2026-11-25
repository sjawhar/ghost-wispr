package concierge

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/dispatch"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// searchLeg is how a fake Dispatch answers one search route.
type searchLeg func(w http.ResponseWriter, r *http.Request)

var (
	// noResults answers 200 with no result.
	noResults searchLeg = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[]}`))
	}
	// noAnswer holds the call until the client gives up: no status comes.
	noAnswer searchLeg = func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}
	// refusedNow answers 503, whole, at once.
	refusedNow searchLeg = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"code":"UNAVAILABLE","error":"search is down"}`))
	}
	// refusedLate answers 503, whole, 50 ms on: within the search's bound.
	refusedLate searchLeg = func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(50 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		refusedNow(w, r)
	}
	// refusedStalled sends a 503's headers, then holds its body until the
	// client gives up (Sol's review of #194, R1).
	refusedStalled searchLeg = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}
)

// TestASearchFailsOnlyWhenDispatchRefuses (R6, R10; Sol's review of #194):
// Sol's matrix, through Answer, with the real Dispatch client against a
// local server. A search that ran out of time with no other result is slow,
// with no error sound; a search Dispatch refused, with no result from the
// other, is an outage, with the error sound (voice.ErrUnreachable), and so is
// a 503 whose body stalls past the bound. A full search that answered decides
// even when the keyword search was refused. At 0081b237 the stalled 503s
// were "slow" with no error: those rows fail there.
func TestASearchFailsOnlyWhenDispatchRefuses(t *testing.T) {
	for _, tc := range []struct {
		name          string
		full, keyword searchLeg
		line          string
		outage        bool
	}{
		{"full past its bound, keyword empty", noAnswer, noResults, searchIsSlow, false},
		{"both past their bound", noAnswer, noAnswer, searchIsSlow, false},
		{"full refused within its bound, keyword empty", refusedLate, noResults, cannotReachDispatch, true},
		{"full past its bound, keyword refused", noAnswer, refusedNow, cannotReachDispatch, true},
		{"both refused", refusedNow, refusedNow, cannotReachDispatch, true},
		{"full refused, keyword past its bound", refusedNow, noAnswer, cannotReachDispatch, true},
		{"full empty, keyword refused", noResults, refusedNow, noIssueFound, false},
		{"both refused, bodies stalled", refusedStalled, refusedStalled, cannotReachDispatch, true},
		{"full refused with its body stalled, keyword empty", refusedStalled, noResults, cannotReachDispatch, true},
		{"full past its bound, keyword refused with its body stalled", noAnswer, refusedStalled, cannotReachDispatch, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			r.concierge.searchBound = 300 * time.Millisecond
			mux := http.NewServeMux()
			mux.HandleFunc("/api/v1/search", tc.full)
			mux.HandleFunc("/api/v1/search/results", tc.keyword)
			mux.HandleFunc("/api/v1/projects", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`[{"key":"ABC"},{"key":"XYZ"},{"key":"DEF"},{"key":"INFRA"}]`))
			})
			server := httptest.NewServer(mux)
			t.Cleanup(server.Close)
			client, err := dispatch.NewClient(server.URL, "test-token", server.Client())
			if err != nil {
				t.Fatal(err)
			}
			r.concierge.mu.Lock()
			r.concierge.records = client
			r.concierge.mu.Unlock()
			var spoken []said
			err = r.concierge.Answer(context.Background(), &r.exchange, "what is happening with the realtime voice", recorder(&spoken))
			if len(spoken) == 0 || spoken[len(spoken)-1].text != tc.line || errors.Is(err, voice.ErrUnreachable) != tc.outage {
				t.Fatalf("Answer = %v, spoken %+v; want %q, and an outage %v", err, spoken, tc.line, tc.outage)
			}
		})
	}
}
