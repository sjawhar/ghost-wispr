// Package googleauthtest provides X.509 workload-identity fixtures for tests.
package googleauthtest

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// X509CredentialFile creates an external-account credential file whose mTLS
// security token service returns issuedToken. It configures the process trust
// store for the fixture server and closes that server when the test ends.
func X509CredentialFile(t *testing.T, issuedToken string) string {
	t.Helper()
	dir := t.TempDir()
	caCertificate, caKey, caPEM := NewCA(t)
	clientCertPEM, clientKeyPEM := NewCertificate(t, caCertificate, caKey, &pkix.Name{CommonName: "ghost-wispr"}, nil, nil, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth})
	serverCertPEM, serverKeyPEM := NewCertificate(t, caCertificate, caKey, &pkix.Name{CommonName: "localhost"}, []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")}, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth})

	caPath := filepath.Join(dir, "ca.pem")
	writeFile(t, caPath, caPEM)
	t.Setenv("SSL_CERT_FILE", caPath)
	clientCertPath := filepath.Join(dir, "client.pem")
	writeFile(t, clientCertPath, clientCertPEM)
	clientKeyPath := filepath.Join(dir, "client-key.pem")
	writeFile(t, clientKeyPath, clientKeyPEM)
	certificateConfigPath := filepath.Join(dir, "certificate-config.json")
	writeJSON(t, certificateConfigPath, map[string]any{
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
	clientCAs := x509.NewCertPool()
	clientCAs.AddCert(caCertificate)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) != 1 || r.TLS.PeerCertificates[0].Subject.CommonName != "ghost-wispr" {
			t.Errorf("STS client certificate = %#v, want ghost-wispr", r.TLS.PeerCertificates)
			http.Error(w, "client certificate required", http.StatusUnauthorized)
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}
		if got := r.Form.Get("subject_token_type"); got != "urn:ietf:params:oauth:token-type:mtls" {
			t.Errorf("subject_token_type = %q, want mTLS", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access" + "_token": issuedToken,
			"expires_in":        3600,
			"token_type":        "Bearer",
		})
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{serverCert}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: clientCAs}
	server.StartTLS()
	t.Cleanup(server.Close)

	credentialsPath := filepath.Join(dir, "credentials.json")
	writeJSON(t, credentialsPath, map[string]any{
		"type":               "external_account",
		"audience":           "//iam.googleapis.com/projects/123/locations/global/workloadIdentityPools/ghost-wispr/providers/x509",
		"subject_token_type": "urn:ietf:params:oauth:token-type:mtls",
		"token_url":          server.URL,
		"credential_source": map[string]any{
			"certificate": map[string]any{
				"certificate_config_location": certificateConfigPath,
			},
		},
	})
	return credentialsPath
}

var sharedCertificateAuthority struct {
	once sync.Once
	cert *x509.Certificate
	key  *rsa.PrivateKey
	pem  []byte
}

// NewCA returns the certificate authority shared by X.509 workload-identity
// fixtures in this test process.
func NewCA(t *testing.T) (*x509.Certificate, *rsa.PrivateKey, []byte) {
	t.Helper()
	sharedCertificateAuthority.once.Do(func() {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		template := &x509.Certificate{
			SerialNumber:          big.NewInt(1),
			Subject:               pkix.Name{CommonName: "Ghost Wispr test CA"},
			NotBefore:             now.Add(-time.Hour),
			NotAfter:              now.Add(time.Hour),
			IsCA:                  true,
			BasicConstraintsValid: true,
			KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		}
		der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
		if err != nil {
			t.Fatal(err)
		}
		certificate, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		sharedCertificateAuthority.cert = certificate
		sharedCertificateAuthority.key = key
		sharedCertificateAuthority.pem = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	})
	return sharedCertificateAuthority.cert, sharedCertificateAuthority.key, sharedCertificateAuthority.pem
}

// NewCertificate creates a certificate signed by ca for an X.509 test fixture.
func NewCertificate(t *testing.T, ca *x509.Certificate, caKey *rsa.PrivateKey, subject *pkix.Name, dnsNames []string, ipAddresses []net.IP, usages []x509.ExtKeyUsage) ([]byte, []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(now.UnixNano()),
		Subject:      *subject,
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(time.Hour),
		DNSNames:     dnsNames,
		IPAddresses:  ipAddresses,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  usages,
	}, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, data)
}
