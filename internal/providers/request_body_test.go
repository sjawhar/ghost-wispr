package providers

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestARefusedNonReplayableRequestStillRenewsTheNextToken catches keeping a
// refused token until expiry just because this request cannot be sent twice.
func TestARefusedNonReplayableRequestStillRenewsTheNextToken(t *testing.T) {
	clk := &clock{t: time.Unix(1_800_000_000, 0)}
	m := &minter{clock: clk}
	var calls atomic.Int32
	var firstAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"input":"x"}` {
			t.Errorf("request body = %q, want the whole input", body)
		}
		if calls.Add(1) == 1 {
			firstAuth = r.Header.Get("Authorization")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Authorization") == firstAuth {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	p, _ := New(gatewayConfig(srv.URL), Options{IDToken: m.IDToken, Now: clk.Now}).Get("gateway")
	client := p.HTTPClient(nil)
	for _, want := range []int{http.StatusUnauthorized, http.StatusOK} {
		// NopCloser hides the reader's replay support, as a streaming body does.
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, io.NopCloser(strings.NewReader(`{"input":"x"}`)))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("status = %d, want %d: a refused token must not survive the first request", resp.StatusCode, want)
		}
	}
	if calls.Load() != 2 || m.n.Load() != 2 {
		t.Fatalf("%d requests, %d token mints; want two logical requests, each with its own token", calls.Load(), m.n.Load())
	}
}
