package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/compute/metadata"
	calendarapi "google.golang.org/api/calendar/v3"
	"google.golang.org/api/option"
)

// dwdAccount is the instance's own service account, as the fake metadata
// server names it, and dwdBotUser the bot user the calendar signs in as.
const (
	dwdAccount = "watcher@example-project.iam.gserviceaccount.com"
	dwdBotUser = "ghost-wispr@meet.example.org"
)

// dwdGoogle answers, in process, the three Google endpoints the delegated
// sign-in reaches: the metadata server, the IAM Credentials API's signJwt,
// and the OAuth token endpoint; it keeps what each was asked.
type dwdGoogle struct {
	mu       sync.Mutex
	metadata []*http.Request
	signJWT  []string // the path of each signJwt request
	claims   []map[string]any
	exchange []url.Values
}

func (g *dwdGoogle) RoundTrip(r *http.Request) (*http.Response, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	answer := func(code int, body string) (*http.Response, error) {
		return &http.Response{
			StatusCode: code,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    r,
		}, nil
	}
	switch {
	case r.URL.Host == "169.254.169.254" && r.URL.Path == "/computeMetadata/v1/instance/service-accounts/default/email":
		g.metadata = append(g.metadata, r)
		if r.Header.Get("Metadata-Flavor") != "Google" {
			return answer(http.StatusForbidden, "missing Metadata-Flavor")
		}
		return answer(http.StatusOK, dwdAccount)
	case r.URL.Host == "iamcredentials.googleapis.com" && strings.HasSuffix(r.URL.Path, ":signJwt"):
		g.signJWT = append(g.signJWT, r.URL.Path)
		var req struct{ Payload string }
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return answer(http.StatusBadRequest, `{"error":{"code":400}}`)
		}
		var claims map[string]any
		if err := json.Unmarshal([]byte(req.Payload), &claims); err != nil {
			return answer(http.StatusBadRequest, `{"error":{"code":400}}`)
		}
		g.claims = append(g.claims, claims)
		return answer(http.StatusOK, `{"keyId":"key-1","signedJwt":"header.payload.signature"}`)
	case r.URL.Host == "oauth2.googleapis.com" && r.URL.Path == "/token":
		body, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(body))
		g.exchange = append(g.exchange, form)
		return answer(http.StatusOK, fmt.Sprintf(`{"access_token":%q,"token_type":"Bearer","expires_in":3600}`, "delegated-token"))
	default:
		return answer(http.StatusNotFound, fmt.Sprintf(`{"error":{"code":404,"message":"no %s %s%s here"}}`, r.Method, r.URL.Host, r.URL.Path))
	}
}

func TestTheCalendarSignsInAsTheBotUserThroughTheInstancesOwnAccount(t *testing.T) {
	t.Setenv("GCE_METADATA_HOST", "")
	google := &dwdGoogle{}
	var authorization []string
	cal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = append(authorization, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"kind":"calendar#events","items":[]}`)
	}))
	t.Cleanup(cal.Close)

	ctx := context.Background()
	svc, err := delegatedCalendarVia(ctx, metadata.NewClient(&http.Client{Transport: google}),
		calendarapi.CalendarReadonlyScope, dwdBotUser,
		[]option.ClientOption{option.WithHTTPClient(&http.Client{Transport: google})},
		[]option.ClientOption{option.WithEndpoint(cal.URL + "/calendar/v3/")})
	if err != nil {
		t.Fatalf("delegatedCalendarVia: %v", err)
	}
	if _, err := svc.Events.List("primary").Context(ctx).Do(); err != nil {
		t.Fatalf("list the calendar: %v", err)
	}

	if len(google.metadata) != 1 {
		t.Fatalf("%d metadata requests, want 1 for the instance's account", len(google.metadata))
	}
	if len(google.signJWT) != 1 {
		t.Fatalf("%d signJwt requests, want 1: %q", len(google.signJWT), google.signJWT)
	}
	if want := "/v1/projects/-/serviceAccounts/" + dwdAccount + ":signJwt"; google.signJWT[0] != want {
		t.Errorf("signJwt went to %q, want %q (the instance's own account signs)", google.signJWT[0], want)
	}
	claims := google.claims[0]
	for claim, want := range map[string]string{
		"iss":   dwdAccount,
		"sub":   dwdBotUser,
		"scope": "https://www.googleapis.com/auth/calendar.readonly",
		"aud":   "https://oauth2.googleapis.com/token",
	} {
		if got, _ := claims[claim].(string); got != want {
			t.Errorf("the signed JWT's %s is %q, want %q", claim, got, want)
		}
	}
	if exp, _ := claims["exp"].(float64); time.Unix(int64(exp), 0).After(time.Now().Add(time.Hour + time.Minute)) {
		t.Errorf("the JWT's exp is %v, more than an hour away", time.Unix(int64(exp), 0))
	}
	if len(google.exchange) != 1 || google.exchange[0].Get("assertion") != "header.payload.signature" {
		t.Errorf("token exchanges %v, want one with the signed JWT as its assertion", google.exchange)
	}
	if len(authorization) != 1 || authorization[0] != "Bearer delegated-token" {
		t.Errorf("the calendar was called with Authorization %q, want [\"Bearer delegated-token\"]", authorization)
	}
}
