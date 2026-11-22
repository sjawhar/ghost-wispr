package googleauth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestIDTokenFromAServiceAccountKeyFile fails if a service-account key file
// as google_credentials_file cannot mint an ID token, or mints one for
// another audience than the one asked for.
func TestIDTokenFromAServiceAccountKeyFile(t *testing.T) {
	want := testIDToken(time.Now().Add(time.Hour))
	var mu sync.Mutex
	var audiences []string
	tokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		parts := strings.Split(r.PostForm.Get("assertion"), ".")
		var claims struct {
			TargetAudience string `json:"target_audience"`
		}
		if len(parts) == 3 {
			payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
			_ = json.Unmarshal(payload, &claims)
		}
		mu.Lock()
		audiences = append(audiences, claims.TargetAudience)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id_token": want})
	}))
	defer tokens.Close()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "service-account.json")
	writeTestJSON(t, path, map[string]any{
		"type":         "service_account",
		"project_id":   "example-project",
		"private_key":  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})),
		"client_email": "ghost-wispr@example-project.iam.gserviceaccount.com",
		"token_uri":    tokens.URL,
	})
	token, err := IDToken(context.Background(), path, "example-gateway")
	mu.Lock()
	defer mu.Unlock()
	if err != nil || token != want || len(audiences) != 1 || audiences[0] != "example-gateway" {
		t.Fatalf("IDToken = %q, %v after asking for audiences %q; want the token for example-gateway", token, err, audiences)
	}
}

// TestIDTokenFromTheMetadataServer fails if a unit on a Compute Engine VM,
// with no google_credentials_file and no other default credentials, cannot
// mint an ID token from the metadata server for its own service account.
func TestIDTokenFromTheMetadataServer(t *testing.T) {
	want := testIDToken(time.Now().Add(time.Hour))
	var mu sync.Mutex
	var asked []string
	metadata := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.URL.Path+"?audience="+r.URL.Query().Get("audience")+" "+r.Header.Get("Metadata-Flavor"))
		mu.Unlock()
		w.Header().Set("Metadata-Flavor", "Google")
		if r.URL.Path != "/computeMetadata/v1/instance/service-accounts/default/identity" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, want)
	}))
	defer metadata.Close()
	u, _ := url.Parse(metadata.URL)
	t.Setenv("GCE_METADATA_HOST", u.Host)
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	t.Setenv("HOME", t.TempDir()) // no gcloud default credentials file
	token, err := IDToken(context.Background(), "", "example-gateway")
	mu.Lock()
	defer mu.Unlock()
	identity := "/computeMetadata/v1/instance/service-accounts/default/identity?audience=example-gateway Google"
	if err != nil || token != want || !containsString(asked, identity) {
		t.Fatalf("IDToken = %q, %v after asking the metadata server %q; want its token for example-gateway", token, err, asked)
	}
}

// TestIDTokenFromUserCredentialsSaysWhatToSet fails if default credentials
// that cannot mint an ID token (a person's gcloud login) fail without naming
// the setting that fixes it.
func TestIDTokenFromUserCredentialsSaysWhatToSet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "application_default_credentials.json")
	writeTestJSON(t, path, map[string]any{"type": "authorized_user", "client_id": "id-1", "client_secret": "s-1", "refresh_token": "r-1"})
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", path)
	_, err := IDToken(context.Background(), "", "example-gateway")
	if err == nil || !strings.Contains(err.Error(), "authorized_user") || !strings.Contains(err.Error(), "set google_credentials_file to the unit's workload identity credential file") {
		t.Fatalf("IDToken = %v; want a refusal naming the credential type and google_credentials_file", err)
	}
}

func containsString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
