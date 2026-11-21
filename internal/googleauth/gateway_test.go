package googleauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/auth"
)

// fakeJWT is an unsigned JWT whose claims say exp and email, as the metadata
// server's ID token in full format carries them.
func fakeJWT(t *testing.T, exp time.Time, email string) string {
	t.Helper()
	claims, err := json.Marshal(map[string]any{"exp": exp.Unix(), "email": email, "aud": "model-gateway-gce"})
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + enc(claims) + "." + enc([]byte("signature"))
}

// TestGatewayCredentialsReadTheMetadataServersIDToken fails unless, with no
// credential file, the gateway's credentials ask the metadata server for an
// ID token for the audience in full format, which carries the box account's
// email that the gateway signs a box in by, and keep it for its life.
func TestGatewayCredentialsReadTheMetadataServersIDToken(t *testing.T) {
	var calls atomic.Int32
	var gotAudience, gotFormat atomic.Value
	token := fakeJWT(t, time.Now().Add(time.Hour), "dev-box@example-project.iam.gserviceaccount.com")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Metadata-Flavor", "Google")
		if !strings.HasSuffix(r.URL.Path, "/instance/service-accounts/default/identity") {
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		gotAudience.Store(r.URL.Query().Get("audience"))
		gotFormat.Store(r.URL.Query().Get("format"))
		_, _ = w.Write([]byte(token))
	}))
	defer srv.Close()
	t.Setenv("GCE_METADATA_HOST", strings.TrimPrefix(srv.URL, "http://"))
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")

	creds, err := GatewayCredentials(context.Background(), "", "model-gateway-gce")
	if err != nil {
		t.Fatalf("GatewayCredentials: %v", err)
	}
	for range 2 {
		got, err := creds.Token(context.Background())
		if err != nil {
			t.Fatalf("Token: %v", err)
		}
		if got.Value != token {
			t.Fatalf("Token = %q, want the metadata server's ID token", got.Value)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("the metadata server was asked %d times for two tokens, want once: the token lives an hour", n)
	}
	if a, _ := gotAudience.Load().(string); a != "model-gateway-gce" {
		t.Errorf("audience = %q, want model-gateway-gce", a)
	}
	if f, _ := gotFormat.Load().(string); f != "full" {
		t.Errorf("format = %q, want full, whose token carries the account's email", f)
	}
}

// TestGatewayCredentialsRefuseACredentialFileThatCannotImpersonate fails if a
// credential file that is no external account gives gateway credentials.
func TestGatewayCredentialsRefuseACredentialFileThatCannotImpersonate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "user.json")
	if err := os.WriteFile(path, []byte(`{"type":"authorized_user","client_id":"x","client_secret":"y","refresh_token":"z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := GatewayCredentials(context.Background(), path, "model-gateway-gce"); err == nil ||
		!strings.Contains(err.Error(), "cannot mint an impersonated ID token") {
		t.Fatalf("GatewayCredentials with an authorized_user file = %v, want the impersonation refusal", err)
	}
}

// sdkDebugHelper is set in the environment of the process
// TestNoCredentialLogsItsTokenAtSDKDebug starts, which then fetches one
// token by the route the variable names and prints "fetched".
const sdkDebugHelper = "GHOST_WISPR_TEST_SDK_DEBUG_ROUTE"

func TestMain(m *testing.M) {
	if route := os.Getenv(sdkDebugHelper); route != "" {
		os.Exit(fetchForSDKDebug(route))
	}
	os.Exit(m.Run())
}

func fetchForSDKDebug(route string) int {
	ctx := context.Background()
	var creds *auth.Credentials
	var err error
	switch route {
	case "gateway":
		creds, err = GatewayCredentials(ctx, "", ModelGatewayAudience)
	case "detect":
		creds, err = Detect(ctx, "", CloudPlatformScope)
	default:
		err = fmt.Errorf("no route %q", route)
	}
	if err == nil {
		var token *auth.Token
		if token, err = creds.Token(ctx); err == nil && token.Value == "" {
			err = fmt.Errorf("an empty token")
		}
	}
	if err != nil {
		fmt.Printf("no token: %v\n", err)
		return 1
	}
	fmt.Println("fetched")
	return 0
}

// TestNoCredentialLogsItsTokenAtSDKDebug fails if, with Google's SDK logging
// at debug (GOOGLE_SDK_GO_LOGGING_LEVEL), a credential of this package writes
// the token it fetched: the gateway's ID token from the metadata server, and
// an access token from Detect. Each run is its own process, since the SDK
// reads the variable once; "fetched" shows the run reached the token.
func TestNoCredentialLogsItsTokenAtSDKDebug(t *testing.T) {
	idToken := fakeJWT(t, time.Now().Add(time.Hour), "dev-box@example-project.iam.gserviceaccount.com")
	fakeAccess := "fake-access-for-the-debug-test"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Metadata-Flavor", "Google")
		switch {
		case strings.HasSuffix(r.URL.Path, "/service-accounts/default/identity"):
			_, _ = w.Write([]byte(idToken))
		case strings.HasSuffix(r.URL.Path, "/service-accounts/default/token"):
			_, _ = fmt.Fprintf(w, `{"access_token":%q,"expires_in":3600,"token_type":"Bearer"}`, fakeAccess)
		default:
			_, _ = w.Write([]byte("example-project"))
		}
	}))
	defer srv.Close()
	for route, token := range map[string]string{"gateway": idToken, "detect": fakeAccess} {
		t.Run(route, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^$")
			cmd.Env = append(os.Environ(),
				sdkDebugHelper+"="+route,
				"GOOGLE_SDK_GO_LOGGING_LEVEL=debug",
				"GCE_METADATA_HOST="+strings.TrimPrefix(srv.URL, "http://"),
				"GOOGLE_APPLICATION_CREDENTIALS=",
				"HOME="+t.TempDir(),
				"CLOUDSDK_CONFIG="+t.TempDir(),
			)
			out, err := cmd.CombinedOutput()
			if err != nil || !strings.Contains(string(out), "fetched") {
				t.Fatalf("the %s run reached no token (%v): %.400s", route, err, out)
			}
			if strings.Contains(string(out), token) {
				t.Errorf("at SDK debug, the %s route's output holds its token", route)
			}
		})
	}
}
