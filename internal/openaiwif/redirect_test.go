package openaiwif

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestAnExchangeRedirectedElsewhereSendsNoSubjectToken fails if the token
// exchange follows a redirect to another origin, so the Google ID token it
// posts reaches a host that is not the exchange's, or if a redirect within the
// exchange's own origin stops working.
func TestAnExchangeRedirectedElsewhereSendsNoSubjectToken(t *testing.T) {
	const subject = "planted-subject-token"
	var elsewhereSaw atomic.Bool
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), subject) {
			elsewhereSaw.Store(true)
		}
		_, _ = io.WriteString(w, `{"access_token":"oa-1","token_type":"Bearer","expires_in":3600}`)
	}))
	defer elsewhere.Close()
	exchange := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/away":
			http.Redirect(w, r, elsewhere.URL+"/token", http.StatusTemporaryRedirect)
		case "/here":
			http.Redirect(w, r, "/token", http.StatusTemporaryRedirect)
		default:
			_, _ = io.WriteString(w, `{"access_token":"oa-1","token_type":"Bearer","expires_in":3600}`)
		}
	}))
	defer exchange.Close()
	source := func(path string) *TokenSource {
		t.Helper()
		s, err := NewTokenSource(&TokenConfig{
			Endpoint:           exchange.URL + path,
			IdentityProviderID: "idp-x",
			ServiceAccountID:   "sa-x",
			Subject:            func(context.Context, string) (string, error) { return subject, nil },
		})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}

	if _, err := source("/away").Token(context.Background()); err == nil {
		t.Error("an exchange redirected to another origin gave a token, want an error")
	}
	if elsewhereSaw.Load() {
		t.Fatal("the other origin received the Google ID token")
	}
	if token, err := source("/here").Token(context.Background()); err != nil || token != "oa-1" {
		t.Fatalf("an exchange redirected within its own origin = %q, %v; want oa-1", token, err)
	}
}
