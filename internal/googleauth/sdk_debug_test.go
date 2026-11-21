package googleauth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// sdkDebugRoute is set in the environment of the process
// TestNoCredentialLogsItsTokenAtSDKDebug starts: it then fetches one token by
// the route the variable names, prints "fetched", and exits.
const sdkDebugRoute = "GHOST_WISPR_TEST_SDK_DEBUG_ROUTE"

// sdkDebugKeyFile is the service-account key file of the "service-account"
// route.
const sdkDebugKeyFile = "GHOST_WISPR_TEST_SDK_DEBUG_KEY_FILE"

func TestMain(m *testing.M) {
	if route := os.Getenv(sdkDebugRoute); route != "" {
		os.Exit(fetchForSDKDebug(route))
	}
	os.Exit(m.Run())
}

// fetchForSDKDebug fetches one token by route, as the unit does.
func fetchForSDKDebug(route string) int {
	ctx := context.Background()
	var err error
	switch route {
	case "metadata id token":
		_, err = IDToken(ctx, "", "example-gateway")
	case "service-account id token":
		_, err = IDToken(ctx, os.Getenv(sdkDebugKeyFile), "example-gateway")
	case "detected access token":
		creds, derr := Detect(ctx, "", CloudPlatformScope)
		if err = derr; err == nil {
			_, err = creds.Token(ctx)
		}
	default:
		err = fmt.Errorf("no route %q", route)
	}
	if err != nil {
		fmt.Printf("no token: %v\n", err)
		return 1
	}
	fmt.Println("fetched")
	return 0
}

// TestNoCredentialLogsItsTokenAtSDKDebug fails if, with Google's SDK logging
// at debug (GOOGLE_SDK_GO_LOGGING_LEVEL=debug), a credential this package
// builds writes the token it fetched to the process's output: the ID token
// from the metadata server (idtoken.NewCredentials), the ID token a
// service-account key signs for (idtoken.NewCredentialsFromJSON), and the
// access token Application Default Credentials fetch (Detect). Each route runs
// in a process of its own, since the SDK reads the variable once; "fetched"
// in its output shows the run reached its token.
func TestNoCredentialLogsItsTokenAtSDKDebug(t *testing.T) {
	idToken := testIDToken(time.Now().Add(time.Hour))
	accessToken := strings.Join([]string{"fake", "access", "debug", "test"}, "-")
	metadata := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Metadata-Flavor", "Google")
		switch {
		case strings.HasSuffix(r.URL.Path, "/service-accounts/default/identity"):
			_, _ = io.WriteString(w, idToken)
		case strings.HasSuffix(r.URL.Path, "/service-accounts/default/token"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"access_token":%q,"expires_in":3600,"token_type":"Bearer"}`, accessToken)
		default:
			_, _ = io.WriteString(w, "example-project")
		}
	}))
	defer metadata.Close()
	signed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id_token": idToken})
	}))
	defer signed.Close()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(t.TempDir(), "service-account.json")
	writeTestJSON(t, keyFile, map[string]any{
		"type":         "service_account",
		"project_id":   "example-project",
		"private_key":  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})),
		"client_email": "ghost-wispr@example-project.iam.gserviceaccount.com",
		"token_uri":    signed.URL,
	})
	host := strings.TrimPrefix(metadata.URL, "http://")
	for route, token := range map[string]string{
		"metadata id token":        idToken,
		"service-account id token": idToken,
		"detected access token":    accessToken,
	} {
		t.Run(route, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^$")
			cmd.Env = append(os.Environ(),
				sdkDebugRoute+"="+route,
				sdkDebugKeyFile+"="+keyFile,
				"GOOGLE_SDK_GO_LOGGING_LEVEL=debug",
				"GCE_METADATA_HOST="+host,
				"GOOGLE_APPLICATION_CREDENTIALS=",
				"HOME="+t.TempDir(),
				"CLOUDSDK_CONFIG="+t.TempDir(),
			)
			out, err := cmd.CombinedOutput()
			if err != nil || !strings.Contains(string(out), "fetched") {
				t.Fatalf("the %s run reached no token (%v): %.600s", route, err, out)
			}
			if strings.Contains(string(out), token) {
				t.Errorf("at SDK debug, the %s run's output holds its token:\n%.1500s", route, out)
			}
		})
	}
}

// TestTheMetadataIDTokenIsTheFullFormat fails if the ID token a unit on a
// Compute Engine VM mints from the metadata server is not asked for in the
// full format (format=full), whose google.compute_engine claims a gateway
// account pinned to its VM is matched on.
func TestTheMetadataIDTokenIsTheFullFormat(t *testing.T) {
	var mu sync.Mutex
	var identity []url.Values
	metadata := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Metadata-Flavor", "Google")
		if !strings.HasSuffix(r.URL.Path, "/service-accounts/default/identity") {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		identity = append(identity, r.URL.Query())
		mu.Unlock()
		_, _ = io.WriteString(w, testIDToken(time.Now().Add(time.Hour)))
	}))
	defer metadata.Close()
	t.Setenv("GCE_METADATA_HOST", strings.TrimPrefix(metadata.URL, "http://"))
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLOUDSDK_CONFIG", t.TempDir())
	if _, err := IDToken(context.Background(), "", "example-gateway"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(identity) != 1 || identity[0].Get("format") != "full" || identity[0].Get("audience") != "example-gateway" {
		t.Fatalf("the metadata server was asked for ID tokens with %v; want one, audience example-gateway and format=full", identity)
	}
}
