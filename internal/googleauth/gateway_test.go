package googleauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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
