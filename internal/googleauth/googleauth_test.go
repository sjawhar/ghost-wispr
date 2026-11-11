package googleauth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/googleauthtest"

	"google.golang.org/api/drive/v3"
)

func TestDetectServiceAccountFile(t *testing.T) {
	dir := t.TempDir()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	credentialPath := filepath.Join(dir, "service-account.json")
	writeTestJSON(t, credentialPath, map[string]any{
		"type":         "service_account",
		"project_id":   "theorem-agent-infra",
		"private_key":  string(keyPEM),
		"client_email": "ghost-wispr@theorem-agent-infra.iam.gserviceaccount.com",
		"token_uri":    "https://oauth2.googleapis.com/token",
	})
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", filepath.Join(dir, "ignored.json"))

	credentials, err := Detect(context.Background(), credentialPath, drive.DriveFileScope)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if credentials.TokenProvider == nil {
		t.Fatal("Detect returned credentials without a token provider")
	}
}

func TestDetectExternalAccountX509(t *testing.T) {
	fixture := newExternalAccountFixture(t)
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", fixture.credentialPath)

	credentials, err := Detect(context.Background(), "", drive.DriveFileScope)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	token, err := credentials.Token(context.Background())
	if err != nil {
		t.Fatalf("credentials.Token: %v", err)
	}
	if token.Value != "impersonated-"+"access-"+"token" {
		t.Fatalf("token = %q, want impersonated access token", token.Value)
	}
	if fixture.clientCN() != "ghost-wispr" {
		t.Fatalf("STS client certificate CN = %q, want ghost-wispr", fixture.clientCN())
	}
}

func TestIDTokenImpersonatesTheServiceAccount(t *testing.T) {
	fixture := newExternalAccountFixture(t)
	fixture.routeIAMRequests(t)

	token, err := IDToken(context.Background(), fixture.credentialPath, "https://tailscale.example.test")
	if err != nil {
		t.Fatalf("IDToken: %v", err)
	}
	if token != fixture.idToken {
		t.Fatalf("IDToken = %q, want %q", token, fixture.idToken)
	}
	if fixture.iamRequests() != 1 {
		t.Fatalf("generateIdToken requests = %d, want 1", fixture.iamRequests())
	}
}

func TestCertExpiryReadsNotAfter(t *testing.T) {
	dir := t.TempDir()
	notAfter := time.Date(2030, 2, 3, 4, 5, 6, 0, time.UTC)
	certPath := writeTestCertificate(t, dir, notAfter)
	certConfigPath := filepath.Join(dir, "certificate-config.json")
	writeTestJSON(t, certConfigPath, map[string]any{
		"cert_configs": map[string]any{
			"workload": map[string]any{
				"cert_path": certPath,
				"key_path":  filepath.Join(dir, "key.pem"),
			},
		},
	})
	credentialsPath := filepath.Join(dir, "credentials.json")
	writeTestJSON(t, credentialsPath, map[string]any{
		"type": "external_account",
		"credential_source": map[string]any{
			"certificate": map[string]any{
				"certificate_config_location": certConfigPath,
			},
		},
	})

	got, ok, err := CertExpiry(credentialsPath)
	if err != nil {
		t.Fatalf("CertExpiry: %v", err)
	}
	if !ok {
		t.Fatal("CertExpiry reported no certificate")
	}
	if !got.Equal(notAfter) {
		t.Fatalf("CertExpiry = %s, want %s", got, notAfter)
	}
}

func TestCertExpiryWithoutCertIsNotOK(t *testing.T) {
	credentialPath := filepath.Join(t.TempDir(), "credentials.json")
	writeTestJSON(t, credentialPath, map[string]any{"type": "service_account"})

	_, ok, err := CertExpiry(credentialPath)
	if err != nil {
		t.Fatalf("CertExpiry: %v", err)
	}
	if ok {
		t.Fatal("CertExpiry reported a certificate for a service account file")
	}
}

func TestStatusWarnsWhenTheCertificateExpiresSoon(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	soon := now.AddDate(0, 0, 10)

	warning := CertificateExpiryWarning(now, soon, true)
	if !strings.Contains(warning, "2026-11-20") {
		t.Fatalf("warning %q does not name the certificate expiry date", warning)
	}
	if !strings.Contains(warning, "docs/pi-setup.md") {
		t.Fatalf("warning %q does not name the renewal runbook", warning)
	}
	if warning := CertificateExpiryWarning(now, now.AddDate(0, 0, 40), true); warning != "" {
		t.Fatalf("warning for certificate expiring in 40 days = %q, want empty", warning)
	}
}

type externalAccountFixture struct {
	credentialPath string
	stsServer      *httptest.Server
	iamServer      *httptest.Server
	caPool         *x509.CertPool
	idToken        string

	mu      sync.Mutex
	cn      string
	iamHits int
}

var sharedTestCertificateAuthority struct {
	once sync.Once
	cert *x509.Certificate
	key  *rsa.PrivateKey
	pem  []byte
}

func sharedTestCA(t *testing.T) (*x509.Certificate, *rsa.PrivateKey, []byte) {
	t.Helper()
	sharedTestCertificateAuthority.once.Do(func() {
		sharedTestCertificateAuthority.cert, sharedTestCertificateAuthority.key, sharedTestCertificateAuthority.pem = googleauthtest.NewCA(t)
	})
	return sharedTestCertificateAuthority.cert, sharedTestCertificateAuthority.key, sharedTestCertificateAuthority.pem
}

func newExternalAccountFixture(t *testing.T) *externalAccountFixture {
	t.Helper()
	dir := t.TempDir()
	caCert, caKey, caPEM := sharedTestCA(t)
	clientCertPEM, clientKeyPEM := googleauthtest.NewCertificate(t, caCert, caKey, &pkix.Name{CommonName: "ghost-wispr"}, nil, nil, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth})
	serverCertPEM, serverKeyPEM := googleauthtest.NewCertificate(t, caCert, caKey, &pkix.Name{CommonName: "iamcredentials.googleapis.com"}, []string{"iamcredentials.googleapis.com", "localhost"}, []net.IP{net.ParseIP("127.0.0.1")}, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth})

	caPath := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caPath, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSL_CERT_FILE", caPath)
	clientCertPath := filepath.Join(dir, "client.pem")
	if err := os.WriteFile(clientCertPath, clientCertPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	clientKeyPath := filepath.Join(dir, "client-key.pem")
	if err := os.WriteFile(clientKeyPath, clientKeyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	certConfigPath := filepath.Join(dir, "certificate-config.json")
	writeTestJSON(t, certConfigPath, map[string]any{
		"cert_configs": map[string]any{
			"workload": map[string]any{
				"cert_path": clientCertPath,
				"key_path":  clientKeyPath,
			},
		},
	})

	serverCert, err := tls.X509KeyPair(serverCertPEM, serverKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &externalAccountFixture{idToken: testIDToken(time.Now().Add(time.Hour))}
	const federatedToken = "federated-" + "access-" + "token"
	const impersonatedToken = "impersonated-" + "access-" + "token"
	fixture.caPool = x509.NewCertPool()
	fixture.caPool.AddCert(caCert)
	fixture.stsServer = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			t.Error("STS request did not present a client certificate")
			http.Error(w, "client certificate required", http.StatusUnauthorized)
			return
		}
		fixture.mu.Lock()
		fixture.cn = r.TLS.PeerCertificates[0].Subject.CommonName
		fixture.mu.Unlock()
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if got := r.Form.Get("subject_token_type"); got != "urn:ietf:params:oauth:token-type:mtls" {
			t.Errorf("subject_token_type = %q, want mTLS", got)
		}
		if got := r.Form.Get("scope"); got != "https://www.googleapis.com/auth/cloud-platform" {
			t.Errorf("scope = %q, want cloud-platform", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access" + "_token": federatedToken,
			"expires_in":        3600,
			"token_type":        "Bearer",
		})
	}))
	fixture.stsServer.TLS = &tls.Config{Certificates: []tls.Certificate{serverCert}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: fixture.caPool}
	fixture.stsServer.StartTLS()
	t.Cleanup(fixture.stsServer.Close)

	fixture.iamServer = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+federatedToken {
			t.Errorf("IAM bearer = %q, want federated access token", got)
			http.Error(w, "wrong bearer", http.StatusUnauthorized)
			return
		}
		const accountPath = "/v1/projects/-/serviceAccounts/ghost-wispr@theorem-agent-infra.iam.gserviceaccount.com"
		switch r.URL.Path {
		case accountPath + ":generateAccessToken":
			_ = json.NewEncoder(w).Encode(map[string]string{
				"access" + "Token": impersonatedToken,
				"expireTime":       "2030-01-02T03:04:05Z",
			})
			return
		case accountPath + ":generateIdToken":
		default:
			t.Errorf("unexpected IAM request path %q", r.URL.Path)
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		var body struct {
			Audience     string `json:"audience"`
			IncludeEmail bool   `json:"includeEmail"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Audience != "https://tailscale.example.test" {
			t.Errorf("audience = %q", body.Audience)
		}
		if !body.IncludeEmail {
			t.Error("includeEmail = false, want true")
		}
		fixture.mu.Lock()
		fixture.iamHits++
		fixture.mu.Unlock()
		_, _ = fmt.Fprintf(w, `{"token":%q}`, fixture.idToken)
	}))
	fixture.iamServer.TLS = &tls.Config{Certificates: []tls.Certificate{serverCert}}
	fixture.iamServer.StartTLS()
	t.Cleanup(fixture.iamServer.Close)

	impersonationURL := fixture.iamServer.URL + "/v1/projects/-/serviceAccounts/ghost-wispr@theorem-agent-infra.iam.gserviceaccount.com:generateAccessToken"
	credentialPath := filepath.Join(dir, "credentials.json")
	writeTestJSON(t, credentialPath, map[string]any{
		"type":                              "external_account",
		"audience":                          "//iam.googleapis.com/projects/123/locations/global/workloadIdentityPools/ghost-wispr/providers/x509",
		"subject_token_type":                "urn:ietf:params:oauth:token-type:mtls",
		"token_url":                         fixture.stsServer.URL,
		"service_account_impersonation_url": impersonationURL,
		"credential_source": map[string]any{
			"certificate": map[string]any{
				"certificate_config_location": certConfigPath,
			},
		},
	})
	fixture.credentialPath = credentialPath
	return fixture
}

func (f *externalAccountFixture) clientCN() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cn
}

func (f *externalAccountFixture) iamRequests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.iamHits
}

func (f *externalAccountFixture) routeIAMRequests(t *testing.T) {
	t.Helper()
	previous := http.DefaultTransport
	transport, ok := previous.(*http.Transport)
	if !ok {
		t.Fatal("http.DefaultTransport is not an *http.Transport")
	}
	mapped := transport.Clone()
	mapped.TLSClientConfig = &tls.Config{RootCAs: f.caPool}
	target, err := url.Parse(f.iamServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	dialer := &net.Dialer{}
	originalDial := mapped.DialContext
	mapped.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address == "iamcredentials.googleapis.com:443" {
			return dialer.DialContext(ctx, network, target.Host)
		}
		if originalDial != nil {
			return originalDial(ctx, network, address)
		}
		return dialer.DialContext(ctx, network, address)
	}
	http.DefaultTransport = mapped
	t.Cleanup(func() { http.DefaultTransport = previous })
}

func testIDToken(expiry time.Time) string {
	payload, _ := json.Marshal(map[string]any{"exp": expiry.Unix()})
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(payload) + "."
}

func writeTestCertificate(t *testing.T, dir string, notAfter time.Time) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "ghost-wispr"},
		NotBefore:    notAfter.Add(-time.Hour),
		NotAfter:     notAfter,
	}, &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "ghost-wispr"},
		NotBefore:    notAfter.Add(-time.Hour),
		NotAfter:     notAfter,
	}, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPath := filepath.Join(dir, "certificate.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath
}

func writeTestJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
