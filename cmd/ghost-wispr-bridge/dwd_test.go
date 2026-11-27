package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
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
// and the OAuth token endpoint; it keeps what each was asked. A test may
// make signJwt or the token exchange refuse with a status, or signJwt
// unreachable.
type dwdGoogle struct {
	mu       sync.Mutex
	metadata []*http.Request
	signJWT  []string // the path of each signJwt request
	claims   []map[string]any
	exchange []url.Values

	signJWTStatus  int  // nonzero: signJwt answers this status
	signJWTDown    bool // signJwt cannot be reached
	exchangeStatus int  // nonzero: the token exchange answers this status
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
		if g.signJWTDown {
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")}
		}
		if g.signJWTStatus != 0 {
			return answer(g.signJWTStatus, `{"error":{"code":403,"message":"Permission 'iam.serviceAccounts.signJwt' denied","status":"PERMISSION_DENIED"}}`)
		}
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
		if g.exchangeStatus != 0 {
			// Google's answer for a subject the account has no domain-wide
			// delegation for.
			return answer(g.exchangeStatus, `{"error":"unauthorized_client","error_description":"Client is unauthorized to retrieve access tokens using this method, or client not authorized for any of the scopes requested."}`)
		}
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

// D5: a refused delegated sign-in, by the token exchange (no domain-wide
// delegation for the bot user) or by signJwt (no permission on the account),
// is logged as its HTTP status and reason=auth, so a reader of
// RESTART-READ-FAILED or WATCHER-READ-FAILED sees the missing delegation. A
// signJwt that cannot be reached stays a network failure.
func TestARefusedDelegatedSignInIsLoggedAsAuth(t *testing.T) {
	t.Setenv("GCE_METADATA_HOST", "")
	for _, tc := range []struct {
		name   string
		google *dwdGoogle
		want   string
	}{
		{"the token exchange refuses the bot user", &dwdGoogle{exchangeStatus: http.StatusUnauthorized}, "status=401 reason=auth"},
		{"signJwt refuses the account", &dwdGoogle{signJWTStatus: http.StatusForbidden}, "status=403 reason=auth"},
		{"signJwt cannot be reached (control)", &dwdGoogle{signJWTDown: true}, "status=none reason=network"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			google := tc.google
			cal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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
			_, err = svc.Events.List("primary").Context(ctx).Do()
			if err == nil {
				t.Fatal("the calendar answered through a sign-in that failed")
			}
			if got := errorClass(err); got != tc.want {
				t.Fatalf("errorClass = %q, want %q", got, tc.want)
			}
		})
	}
}

// sdkLogChildMode is set in the environment of each process that
// TestGoogleSDKLoggingIsWithheldAtEachModesStart starts: the mode whose start
// TestGoogleSDKLoggingChild runs first, "" for none.
const sdkLogChildMode = "GHOST_WISPR_TEST_SDK_LOG_MODE"

// The words of the one event the child's fake calendar answers with: in the
// process's output, only Google's SDK logger writes them.
const (
	sdkLogTitle       = "quokka-meeting-title-canary"
	sdkLogDescription = "quokka-meeting-description-canary"
)

// TestGoogleSDKLoggingChild is the process that
// TestGoogleSDKLoggingIsWithheldAtEachModesStart starts. It runs the mode
// sdkLogChildMode names with no settings, through run, which ends at the
// settings' refusal; then it reads a calendar of one event through the
// delegated sign-in, and prints "SDK-LOG-CHILD listed".
func TestGoogleSDKLoggingChild(t *testing.T) {
	mode, ok := os.LookupEnv(sdkLogChildMode)
	if !ok {
		t.Skip("the process that TestGoogleSDKLoggingIsWithheldAtEachModesStart starts")
	}
	if mode != "" {
		var stdout, stderr strings.Builder
		if code := run(t.Context(), []string{mode}, func(string) string { return "" }, &stdout, &stderr); code != 2 || !strings.Contains(stdout.String(), "SETTINGS-REFUSED") {
			t.Fatalf("the %s mode with no settings exited %d, want 2 at SETTINGS-REFUSED; stdout %q stderr %q", mode, code, stdout.String(), stderr.String())
		}
	}
	t.Setenv("GCE_METADATA_HOST", "")
	google := &dwdGoogle{}
	cal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"kind":"calendar#events","items":[{"id":"event1","summary":%q,"description":%q}]}`, sdkLogTitle, sdkLogDescription)
	}))
	defer cal.Close()
	ctx := context.Background()
	svc, err := delegatedCalendarVia(ctx, metadata.NewClient(&http.Client{Transport: google}),
		calendarapi.CalendarReadonlyScope, dwdBotUser,
		[]option.ClientOption{option.WithHTTPClient(&http.Client{Transport: google})},
		[]option.ClientOption{option.WithEndpoint(cal.URL + "/calendar/v3/")})
	if err != nil {
		t.Fatalf("delegatedCalendarVia: %v", err)
	}
	evs, err := svc.Events.List("primary").Context(ctx).Do()
	if err != nil {
		t.Fatalf("list the calendar: %v", err)
	}
	if len(evs.Items) != 1 || evs.Items[0].Summary != sdkLogTitle {
		t.Fatalf("the calendar gave %d events, want the fake's one", len(evs.Items))
	}
	fmt.Println("SDK-LOG-CHILD listed")
}

// GOOGLE_SDK_GO_LOGGING_LEVEL=debug makes Google's SDK logger write each
// answer whole to stderr, around the marked lines: an event's title and
// description. Each mode's start, run, turns that logging off before the
// mode reads its settings, and so before any Google client. Each case is a
// process of its own, as the SDK reads the variable when a client is built.
// The controls: with no mode's start, the SDK at debug writes both words;
// with its logging off, neither. (Sol's second review of #202 at 2e217ba6,
// TestReviewCalendarDebugLogsNeverExposeMeetingWords.)
func TestGoogleSDKLoggingIsWithheldAtEachModesStart(t *testing.T) {
	for _, tc := range []struct {
		name, level, mode string
		words             bool // the SDK's logger writes the event's words
	}{
		{"SDK debug, no mode's start (control)", "debug", "", true},
		{"SDK logging off (control)", "", "", false},
		{"bridge mode at SDK debug", "debug", "bridge", false},
		{"watch mode at SDK debug", "debug", "watch", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestGoogleSDKLoggingChild$")
			for _, kv := range os.Environ() {
				if !strings.HasPrefix(kv, "GOOGLE_SDK_GO_LOGGING_LEVEL=") && !strings.HasPrefix(kv, sdkLogChildMode+"=") {
					cmd.Env = append(cmd.Env, kv)
				}
			}
			cmd.Env = append(cmd.Env, sdkLogChildMode+"="+tc.mode, "GOOGLE_SDK_GO_LOGGING_LEVEL="+tc.level)
			out, err := cmd.CombinedOutput()
			if err != nil || !strings.Contains(string(out), "SDK-LOG-CHILD listed") {
				t.Fatalf("the process read no calendar (%v):\n%s", err, out)
			}
			var withWords []string
			title, description := false, false
			for line := range strings.Lines(string(out)) {
				if strings.Contains(line, sdkLogTitle) || strings.Contains(line, sdkLogDescription) {
					withWords = append(withWords, line)
					title = title || strings.Contains(line, sdkLogTitle)
					description = description || strings.Contains(line, sdkLogDescription)
				}
			}
			switch {
			case tc.words && (!title || !description):
				t.Fatalf("control: the SDK at debug wrote title %v, description %v; the check cannot see its logger:\n%s", title, description, out)
			case !tc.words && len(withWords) > 0:
				t.Fatalf("the SDK's logger wrote the event's words around the marked lines:\n%s", strings.Join(withWords, ""))
			}
		})
	}
}
