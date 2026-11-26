package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/googleauthtest"
)

type secretAccess struct {
	project string
	secret  string
	version string
}

type secretReply struct {
	value secretVersion
	err   error
}

type fakeSecretReader struct {
	replies map[string]secretReply
	calls   []secretAccess
}

func (r *fakeSecretReader) Access(_ context.Context, project, secret, version string) (secretVersion, error) {
	r.calls = append(r.calls, secretAccess{project: project, secret: secret, version: version})
	reply, ok := r.replies[secretReplyKey(secret, version)]
	if !ok {
		return secretVersion{}, fmt.Errorf("unexpected access to %s version %s", secret, version)
	}
	return reply.value, reply.err
}

func secretReplyKey(secret, version string) string { return secret + "\x00" + version }

func readerFactory(t *testing.T, reader *fakeSecretReader, wantCredentials string) func(context.Context, string) (secretReader, error) {
	t.Helper()
	return func(_ context.Context, credentials string) (secretReader, error) {
		if credentials != wantCredentials {
			t.Errorf("reader credentials = %q, want %q", credentials, wantCredentials)
		}
		return reader, nil
	}
}

func defaultCredentials(path string) func() (string, error) {
	return func() (string, error) { return path, nil }
}

func requireAccesses(t *testing.T, reader *fakeSecretReader, want []secretAccess) {
	t.Helper()
	if !reflect.DeepEqual(reader.calls, want) {
		t.Fatalf("secret accesses = %#v, want %#v", reader.calls, want)
	}
}

func requireFileAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s exists or could not be read: %v", path, err)
	}
}

// TestPiSecretsWritesEachEnvSecretAndPrintsNoValue catches a writer that
// reorders flags, preserves the required trailing LF, prints a secret, or
// creates the environment file with group or world permissions.
func TestPiSecretsWritesEachEnvSecretAndPrintsNoValue(t *testing.T) {
	const credentials = "credential-file"
	reader := &fakeSecretReader{replies: map[string]secretReply{
		secretReplyKey("first-secret", "latest"):  {value: secretVersion{Version: 3, Data: []byte("first-value\n")}},
		secretReplyKey("second-secret", "latest"): {value: secretVersion{Version: 4, Data: []byte("second-value")}},
	}}
	outDir := t.TempDir()
	var output bytes.Buffer

	code := runPiSecrets(
		context.Background(),
		[]string{"pi-secrets", "--project", "example-project", "--out", outDir, "--env-secret", "FIRST=first-secret", "--env-secret", "SECOND=second-secret"},
		&output,
		defaultCredentials(credentials),
		readerFactory(t, reader, credentials),
		func(context.Context, string) error {
			t.Fatal("environment-only secret read signed in")
			return nil
		},
	)
	if code != 0 {
		t.Fatalf("runPiSecrets exit = %d, want 0", code)
	}

	path := filepath.Join(outDir, "secrets.env")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	const want = "FIRST=first-value\nSECOND=second-value\n"
	if string(got) != want {
		t.Fatalf("secrets.env = %q, want %q", got, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if gotMode := info.Mode().Perm(); gotMode != 0o600 {
		t.Errorf("secrets.env mode = %#o, want 0600", gotMode)
	}

	const wantOutput = "pi-secrets: FIRST from first-secret version 3\npi-secrets: SECOND from second-secret version 4\n"
	if gotOutput := output.String(); gotOutput != wantOutput {
		t.Errorf("stdout = %q, want %q", gotOutput, wantOutput)
	}
	for _, value := range []string{"first-value", "second-value"} {
		if strings.Contains(output.String(), value) {
			t.Errorf("stdout contains secret value %q: %q", value, output.String())
		}
	}
	requireAccesses(t, reader, []secretAccess{
		{project: "example-project", secret: "first-secret", version: "latest"},
		{project: "example-project", secret: "second-secret", version: "latest"},
	})
}

// requireEnvironmentValueRefused runs pi-secrets on one environment secret
// whose value is value. It fails t unless the command exits 1, writes no
// secrets.env, and prints no marker, a word of the value.
func requireEnvironmentValueRefused(t *testing.T, value []byte, marker string) {
	t.Helper()
	reader := &fakeSecretReader{replies: map[string]secretReply{
		secretReplyKey("environment-secret", "latest"): {value: secretVersion{Version: 1, Data: value}},
	}}
	outDir := t.TempDir()
	var output bytes.Buffer
	code := runPiSecrets(
		context.Background(),
		[]string{"pi-secrets", "--project", "example-project", "--out", outDir, "--env-secret", "SAFE=environment-secret"},
		&output,
		defaultCredentials("credential-file"),
		readerFactory(t, reader, "credential-file"),
		func(context.Context, string) error { return nil },
	)
	if code != 1 {
		t.Fatalf("runPiSecrets exit = %d, want 1", code)
	}
	requireFileAbsent(t, filepath.Join(outDir, "secrets.env"))
	if marker != "" && strings.Contains(output.String(), marker) {
		t.Errorf("stdout contains the refused value's %q: %q", marker, output.String())
	}
}

// requireEnvironmentValueAccepted runs pi-secrets on one environment secret
// whose value is value. It fails t unless the command exits 0, writes
// secrets.env as want, and prints no marker, a word of the value.
func requireEnvironmentValueAccepted(t *testing.T, value []byte, want, marker string) {
	t.Helper()
	reader := &fakeSecretReader{replies: map[string]secretReply{
		secretReplyKey("environment-secret", "latest"): {value: secretVersion{Version: 1, Data: value}},
	}}
	outDir := t.TempDir()
	var output bytes.Buffer
	code := runPiSecrets(
		context.Background(),
		[]string{"pi-secrets", "--project", "example-project", "--out", outDir, "--env-secret", "SAFE=environment-secret"},
		&output,
		defaultCredentials("credential-file"),
		readerFactory(t, reader, "credential-file"),
		func(context.Context, string) error { return nil },
	)
	if code != 0 {
		t.Fatalf("runPiSecrets exit = %d, want 0", code)
	}
	got, err := os.ReadFile(filepath.Join(outDir, "secrets.env"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("secrets.env = %q, want %q", got, want)
	}
	if strings.Contains(output.String(), marker) {
		t.Errorf("stdout contains the value's %q: %q", marker, output.String())
	}
}

// TestPiSecretsRefusesAValueThatWouldAddASetting catches validation that lets
// a line feed inside a value start a second line of secrets.env, which the
// unit would read as another setting. The ordinary value is its control: a
// check that refused every value would fail it.
func TestPiSecretsRefusesAValueThatWouldAddASetting(t *testing.T) {
	requireEnvironmentValueRefused(t, []byte("injected-value\nGHOST_WISPR_CONFIG=/tmp/x"), "injected-value")
	requireEnvironmentValueAccepted(t, []byte("ordinary-value"), "SAFE=ordinary-value\n", "ordinary-value")
}

// TestPiSecretsRefusesAnEmptyOrControlValue catches validation that writes an
// empty setting, checks for emptiness before it removes the final line feed,
// or keeps a carriage return or a NUL in a value. A value whose one line feed
// is its last byte is the control: pi-secrets removes that line feed and
// accepts the value.
func TestPiSecretsRefusesAnEmptyOrControlValue(t *testing.T) {
	for _, tc := range []struct {
		name   string
		value  []byte
		marker string
	}{
		{name: "empty", value: []byte("")},
		{name: "only a line feed", value: []byte("\n")},
		{name: "carriage return", value: []byte("control-value\r\n"), marker: "control-value"},
		{name: "NUL", value: []byte("control-value\x00"), marker: "control-value"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireEnvironmentValueRefused(t, tc.value, tc.marker)
		})
	}
	t.Run("a final line feed alone", func(t *testing.T) {
		requireEnvironmentValueAccepted(t, []byte("control-value\n"), "SAFE=control-value\n", "control-value")
	})
}

// TestPiSecretsRefusesABadChecksum catches a reader or a command that writes a
// value whose CRC32C does not match the checksum Secret Manager sent with it.
// It runs the real Secret Manager reader over a fake transport. A checksum of
// the right CRC32C plus 2^32 catches a compare that truncates the checksum to
// 32 bits, and the right CRC32C with a minus sign catches a parse that drops
// the sign. The same answer with its right checksum is the control.
func TestPiSecretsRefusesABadChecksum(t *testing.T) {
	data := []byte("checksum-value")
	crc := crc32.Checksum(data, crc32.MakeTable(crc32.Castagnoli))
	for _, tc := range []struct {
		name     string
		checksum string
		wantCode int
	}{
		{name: "wrong checksum", checksum: fmt.Sprint(crc ^ 1), wantCode: 1},
		{name: "right checksum plus 2^32", checksum: fmt.Sprint(uint64(crc) + 1<<32), wantCode: 1},
		{name: "negative right checksum", checksum: "-" + fmt.Sprint(crc), wantCode: 1},
		{name: "right checksum", checksum: fmt.Sprint(crc), wantCode: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{
				"name": "projects/example-project/secrets/checksum-secret/versions/4",
				"payload": map[string]string{
					"data":       base64.StdEncoding.EncodeToString(data),
					"dataCrc32c": tc.checksum,
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			reader := newTestSecretManagerReader(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}, nil
			})
			outDir := t.TempDir()
			var output bytes.Buffer

			code := runPiSecrets(
				context.Background(),
				[]string{"pi-secrets", "--project", "example-project", "--out", outDir, "--env-secret", "SAFE=checksum-secret"},
				&output,
				defaultCredentials("credential-file"),
				func(context.Context, string) (secretReader, error) { return reader, nil },
				func(context.Context, string) error { return nil },
			)
			if code != tc.wantCode {
				t.Fatalf("runPiSecrets exit = %d, want %d", code, tc.wantCode)
			}
			if strings.Contains(output.String(), "checksum-value") {
				t.Errorf("stdout contains the secret value: %q", output.String())
			}
			path := filepath.Join(outDir, "secrets.env")
			if tc.wantCode != 0 {
				requireFileAbsent(t, path)
				return
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != "SAFE=checksum-value\n" {
				t.Errorf("secrets.env = %q, want SAFE=checksum-value", got)
			}
		})
	}
}

// TestPiSecretsWritesNothingWhenOneReadFails catches a writer that commits an
// incomplete secrets.env after an earlier read succeeded, and a failed read or
// check that skips the certificate step: R10 keeps that step running, so the
// newer certificate that signs in is still staged while the exit code stays 1.
func TestPiSecretsWritesNothingWhenOneReadFails(t *testing.T) {
	for _, tc := range []struct {
		name   string
		second secretReply
	}{
		{name: "a read fails", second: secretReply{err: errors.New("second secret unavailable")}},
		{name: "a check fails", second: secretReply{value: secretVersion{Version: 2, Data: []byte("second-value\nINJECTED=1")}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newCertificateFixture(t)
			reader := &fakeSecretReader{replies: map[string]secretReply{
				secretReplyKey("first-secret", "latest"):  {value: secretVersion{Version: 1, Data: []byte("first-value")}},
				secretReplyKey("second-secret", "latest"): tc.second,
				secretReplyKey(fixture.secret, "latest"):  {value: secretVersion{Version: 3, Data: fixture.candidate}},
			}}
			args := append(fixture.args(), "--env-secret", "FIRST=first-secret", "--env-secret", "SECOND=second-secret")
			var output bytes.Buffer

			code := runPiSecrets(
				context.Background(),
				args,
				&output,
				defaultCredentials(fixture.credentials),
				readerFactory(t, reader, fixture.credentials),
				productionSignInCheck,
			)
			if code != 1 {
				t.Fatalf("runPiSecrets exit = %d, want 1", code)
			}
			requireFileAbsent(t, filepath.Join(fixture.outDir, "secrets.env"))
			for _, value := range []string{"first-value", "second-value"} {
				if strings.Contains(output.String(), value) {
					t.Errorf("stdout contains secret value %q: %q", value, output.String())
				}
			}
			staged, err := os.ReadFile(filepath.Join(fixture.outDir, "device.crt"))
			if err != nil {
				t.Fatalf("the certificate step did not stage the newer certificate: %v", err)
			}
			if !bytes.Equal(staged, fixture.candidate) {
				t.Errorf("device.crt = %q, want certificate version 3", staged)
			}
			requireOneOutputLine(t, output.String(), "pi-secrets: certificate version 3, not after ", ": newer and signs in")
		})
	}
}

// requireOneOutputLine fails t unless exactly one line of output starts with
// prefix and ends with suffix.
func requireOneOutputLine(t *testing.T, output, prefix, suffix string) {
	t.Helper()
	matches := 0
	for line := range strings.SplitSeq(output, "\n") {
		if strings.HasPrefix(line, prefix) && strings.HasSuffix(line, suffix) {
			matches++
		}
	}
	if matches != 1 {
		t.Errorf("stdout has %d lines %q...%q, want 1: %q", matches, prefix, suffix, output)
	}
}

// secretReaderFunc is a secretReader made of one function, for a reader whose
// answer depends on the context it gets.
type secretReaderFunc func(ctx context.Context, project, secret, version string) (secretVersion, error)

func (f secretReaderFunc) Access(ctx context.Context, project, secret, version string) (secretVersion, error) {
	return f(ctx, project, secret, version)
}

// TestPiSecretsEndsWithin30s catches a command with no bound of its own, and a
// bound that covers the key reads but not the certificate step: a Secret
// Manager read that never answers would hold update.sh's run. Each read blocks
// until its context ends, and the test's context has no deadline, so only
// pi-secrets' own bound can end it. A read still blocked after a minute gives
// up and fails the test. Each case runs in a synctest bubble, whose fake clock
// passes the 30 s at once. The key file and the installed certificate are made
// outside the bubbles.
func TestPiSecretsEndsWithin30s(t *testing.T) {
	ca, caKey, _ := googleauthtest.NewCA(t)
	key := newRSAKey(t)
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "device.key")
	writePrivateKey(t, keyPath, key)
	installedPath := filepath.Join(dir, "installed.crt")
	writeTestFile(t, installedPath, certificateForKey(t, ca, caKey, key, time.Now().Add(24*time.Hour)))

	for _, tc := range []struct {
		name         string
		certificate  []string
		wantAccesses []secretAccess
		wantLine     string
	}{
		{
			name:         "a key read",
			wantAccesses: []secretAccess{{project: "example-project", secret: "hanging-secret", version: "latest"}},
		},
		{
			name:        "a key read and the certificate step",
			certificate: []string{"--certificate-secret", "hanging-certificate", "--key", keyPath, "--installed-certificate", installedPath},
			wantAccesses: []secretAccess{
				{project: "example-project", secret: "hanging-secret", version: "latest"},
				{project: "example-project", secret: "hanging-certificate", version: "latest"},
			},
			wantLine: "pi-secrets: certificate kept: latest certificate cannot be read\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			outDir := t.TempDir()
			args := append([]string{"pi-secrets", "--project", "example-project", "--out", outDir, "--env-secret", "SAFE=hanging-secret"}, tc.certificate...)
			synctest.Test(t, func(t *testing.T) {
				var accesses []secretAccess
				var ended []error
				reader := secretReaderFunc(func(ctx context.Context, project, secret, version string) (secretVersion, error) {
					accesses = append(accesses, secretAccess{project: project, secret: secret, version: version})
					var err error
					select {
					case <-ctx.Done():
						err = ctx.Err()
					case <-time.After(time.Minute):
						err = errors.New("no bound ended the read within a minute")
					}
					ended = append(ended, err)
					return secretVersion{}, err
				})
				var output bytes.Buffer
				start := time.Now()

				code := runPiSecrets(
					context.Background(),
					args,
					&output,
					defaultCredentials("credential-file"),
					func(context.Context, string) (secretReader, error) { return reader, nil },
					func(context.Context, string) error {
						t.Error("a certificate that no read returned reached the sign-in")
						return nil
					},
				)
				elapsed := time.Since(start)
				if elapsed > 30*time.Second {
					t.Errorf("pi-secrets ran %v, want at most 30s", elapsed)
				}
				if code != 1 {
					t.Errorf("runPiSecrets exit = %d, want 1", code)
				}
				if !reflect.DeepEqual(accesses, tc.wantAccesses) {
					t.Errorf("secret accesses = %#v, want %#v", accesses, tc.wantAccesses)
				}
				for i, err := range ended {
					if !errors.Is(err, context.DeadlineExceeded) {
						t.Errorf("read %d ended with %v, want pi-secrets' own deadline", i+1, err)
					}
				}
				if tc.wantLine != "" && strings.Count(output.String(), tc.wantLine) != 1 {
					t.Errorf("stdout = %q, want the line %q once", output.String(), tc.wantLine)
				}
				requireFileAbsent(t, filepath.Join(outDir, "secrets.env"))
				requireFileAbsent(t, filepath.Join(outDir, "device.crt"))
			})
		})
	}
}

type certificateFixture struct {
	credentials string
	project     string
	secret      string
	outDir      string
	keyPath     string
	installed   string
	candidate   []byte
	key         *rsa.PrivateKey
	ca          *x509.Certificate
	caKey       *rsa.PrivateKey
}

func newCertificateFixture(t *testing.T) certificateFixture {
	t.Helper()
	credentials := googleauthtest.X509CredentialFile(t, "sign-in-token")
	ca, caKey, _ := googleauthtest.NewCA(t)
	key := newRSAKey(t)
	now := time.Now().UTC().Truncate(time.Second)
	candidate := certificateForKey(t, ca, caKey, key, now.Add(48*time.Hour))
	installed := certificateForKey(t, ca, caKey, newRSAKey(t), now.Add(24*time.Hour))

	dir := t.TempDir()
	outDir := filepath.Join(dir, "out")
	if err := os.Mkdir(outDir, 0o700); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "device.key")
	writePrivateKey(t, keyPath, key)
	installedPath := filepath.Join(dir, "installed.crt")
	writeTestFile(t, installedPath, installed)

	return certificateFixture{
		credentials: credentials,
		project:     "example-project",
		secret:      "certificate-secret",
		outDir:      outDir,
		keyPath:     keyPath,
		installed:   installedPath,
		candidate:   candidate,
		key:         key,
		ca:          ca,
		caKey:       caKey,
	}
}

func (f *certificateFixture) args() []string {
	return []string{
		"pi-secrets",
		"--project", f.project,
		"--out", f.outDir,
		"--certificate-secret", f.secret,
		"--key", f.keyPath,
		"--installed-certificate", f.installed,
	}
}

func newRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func certificateForKey(t *testing.T, ca *x509.Certificate, caKey *rsa.PrivateKey, key *rsa.PrivateKey, notAfter time.Time) []byte {
	t.Helper()
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(notAfter.UnixNano()),
		Subject:      pkix.Name{CommonName: "ghost-wispr"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func writePrivateKey(t *testing.T, path string, key *rsa.PrivateKey) {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func writeTestFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func newUntrustedCertificateAuthority(t *testing.T) (*x509.Certificate, *rsa.PrivateKey) {
	t.Helper()
	key := newRSAKey(t)
	now := time.Now()
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber:          big.NewInt(now.UnixNano()),
		Subject:               pkix.Name{CommonName: "test certificate authority"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}, &x509.Certificate{
		SerialNumber:          big.NewInt(now.UnixNano()),
		Subject:               pkix.Name{CommonName: "test certificate authority"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return certificate, key
}

func signInWithClientCertificateEnvironment(t *testing.T, want string, workspace *string) signInCheck {
	t.Helper()
	return func(ctx context.Context, credentialsFile string) error {
		if workspace != nil {
			*workspace = filepath.Dir(credentialsFile)
		}
		if got := os.Getenv(piSecretsClientCertificate); got != want {
			return fmt.Errorf("%s = %q, want %q", piSecretsClientCertificate, got, want)
		}
		return productionSignInCheck(ctx, credentialsFile)
	}
}

func certificateReader(secret string, version int, certificate []byte) *fakeSecretReader {
	return &fakeSecretReader{replies: map[string]secretReply{
		secretReplyKey(secret, "latest"): {value: secretVersion{Version: version, Data: certificate}},
	}}
}

func requireCertificateStaged(t *testing.T, output *bytes.Buffer, outDir string, version int, want []byte) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(outDir, "device.crt"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("device.crt = %q, want certificate version %d", got, version)
	}
	prefix := fmt.Sprintf("pi-secrets: certificate version %d, not after ", version)
	if !strings.HasPrefix(output.String(), prefix) || !strings.HasSuffix(output.String(), ": newer and signs in\n") {
		t.Errorf("certificate stdout = %q, want %q followed by its date and newer-and-signs-in status", output.String(), prefix)
	}
	if strings.Contains(output.String(), string(want)) {
		t.Errorf("certificate stdout contains the certificate value")
	}
}

// TestPiSecretsStagesANewerCertificateThatSignsIn catches a certificate
// installer that accepts a stale certificate, a different key, or a candidate
// that cannot complete the real X.509 federation sign-in.
func TestPiSecretsStagesANewerCertificateThatSignsIn(t *testing.T) {
	fixture := newCertificateFixture(t)
	reader := certificateReader(fixture.secret, 3, fixture.candidate)
	var workspace string
	var output bytes.Buffer

	code := runPiSecrets(
		context.Background(),
		fixture.args(),
		&output,
		defaultCredentials(fixture.credentials),
		readerFactory(t, reader, fixture.credentials),
		func(ctx context.Context, credentialsFile string) error {
			if credentialsFile == fixture.credentials {
				return errors.New("sign-in did not use temporary credentials")
			}
			workspace = filepath.Dir(credentialsFile)
			return productionSignInCheck(ctx, credentialsFile)
		},
	)
	if code != 0 {
		t.Fatalf("runPiSecrets exit = %d, want 0", code)
	}
	requireCertificateStaged(t, &output, fixture.outDir, 3, fixture.candidate)
	requireAccesses(t, reader, []secretAccess{{project: fixture.project, secret: fixture.secret, version: "latest"}})
	if workspace == "" {
		t.Fatal("sign-in did not receive a temporary credential workspace")
	}
	requireFileAbsent(t, workspace)
}

// TestPiSecretsFindsTheRealCertificateBehindABadNewerOne catches a renewal
// reader that stops at an unusable latest version, treats a disabled
// (FAILED_PRECONDITION) or absent (NOT_FOUND) version as the end of the walk,
// reads a version list, or stages a candidate that failed its real sign-in.
func TestPiSecretsFindsTheRealCertificateBehindABadNewerOne(t *testing.T) {
	fixture := newCertificateFixture(t)
	badCA, badCAKey := newUntrustedCertificateAuthority(t)
	badNewer := certificateForKey(t, badCA, badCAKey, fixture.key, time.Now().Add(72*time.Hour))
	goodOlder := certificateForKey(t, fixture.ca, fixture.caKey, fixture.key, time.Now().Add(60*time.Hour))
	reader := &fakeSecretReader{replies: map[string]secretReply{
		secretReplyKey(fixture.secret, "latest"): {value: secretVersion{Version: 7, Data: badNewer}},
		secretReplyKey(fixture.secret, "6"):      {err: secretManagerStatus("FAILED_PRECONDITION")},
		secretReplyKey(fixture.secret, "5"):      {err: secretManagerStatus("NOT_FOUND")},
		secretReplyKey(fixture.secret, "4"):      {value: secretVersion{Version: 4, Data: goodOlder}},
	}}
	var output bytes.Buffer

	code := runPiSecrets(
		context.Background(),
		fixture.args(),
		&output,
		defaultCredentials(fixture.credentials),
		readerFactory(t, reader, fixture.credentials),
		productionSignInCheck,
	)
	if code != 0 {
		t.Fatalf("runPiSecrets exit = %d, want 0", code)
	}
	requireCertificateStaged(t, &output, fixture.outDir, 4, goodOlder)
	requireAccesses(t, reader, []secretAccess{
		{project: fixture.project, secret: fixture.secret, version: "latest"},
		{project: fixture.project, secret: fixture.secret, version: "6"},
		{project: fixture.project, secret: fixture.secret, version: "5"},
		{project: fixture.project, secret: fixture.secret, version: "4"},
	})
}

// TestPiSecretsStopsTheWalkOnPermissionDenied catches a version walk that
// skips a refused version as if it were disabled or deleted: only
// FAILED_PRECONDITION and NOT_FOUND are skipped. Version 3 holds a certificate
// that would sign in, so a walk that continued past the 403 would stage it.
func TestPiSecretsStopsTheWalkOnPermissionDenied(t *testing.T) {
	fixture := newCertificateFixture(t)
	installedBefore, err := os.ReadFile(fixture.installed)
	if err != nil {
		t.Fatal(err)
	}
	badCA, badCAKey := newUntrustedCertificateAuthority(t)
	badNewer := certificateForKey(t, badCA, badCAKey, fixture.key, time.Now().Add(72*time.Hour))
	goodOlder := certificateForKey(t, fixture.ca, fixture.caKey, fixture.key, time.Now().Add(60*time.Hour))
	denied := newSecretManagerError(http.StatusForbidden, []byte(`{"error":{"code":403,"message":"Permission denied on resource","status":"PERMISSION_DENIED"}}`))
	reader := &fakeSecretReader{replies: map[string]secretReply{
		secretReplyKey(fixture.secret, "latest"): {value: secretVersion{Version: 5, Data: badNewer}},
		secretReplyKey(fixture.secret, "4"):      {err: denied},
		secretReplyKey(fixture.secret, "3"):      {value: secretVersion{Version: 3, Data: goodOlder}},
	}}
	var output bytes.Buffer

	code := runPiSecrets(
		context.Background(),
		fixture.args(),
		&output,
		defaultCredentials(fixture.credentials),
		readerFactory(t, reader, fixture.credentials),
		productionSignInCheck,
	)
	if code != 0 {
		t.Fatalf("runPiSecrets exit = %d, want 0", code)
	}
	requireFileAbsent(t, filepath.Join(fixture.outDir, "device.crt"))
	requireKeptCertificate(t, output.String(), 5, badNewer, "candidate does not sign in")
	requireAccesses(t, reader, []secretAccess{
		{project: fixture.project, secret: fixture.secret, version: "latest"},
		{project: fixture.project, secret: fixture.secret, version: "4"},
	})
	installedAfter, err := os.ReadFile(fixture.installed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(installedAfter, installedBefore) {
		t.Error("the installed certificate changed")
	}
}

// TestPiSecretsReadsAtMostTenCertificateVersions catches a walk that reads
// more than the latest version and the nine under it. Latest is version 12
// and versions 11 to 3 are absent, so an eleventh read, of version 2, fails it.
func TestPiSecretsReadsAtMostTenCertificateVersions(t *testing.T) {
	fixture := newCertificateFixture(t)
	mismatchedKey := newRSAKey(t)
	mismatchedCertificate := certificateForKey(t, fixture.ca, fixture.caKey, mismatchedKey, time.Now().Add(72*time.Hour))
	reader := &fakeSecretReader{replies: map[string]secretReply{
		secretReplyKey(fixture.secret, "latest"): {value: secretVersion{Version: 12, Data: mismatchedCertificate}},
	}}
	for version := 11; version >= 3; version-- {
		reader.replies[secretReplyKey(fixture.secret, fmt.Sprint(version))] = secretReply{err: secretManagerStatus("NOT_FOUND")}
	}
	var output bytes.Buffer

	code := runPiSecrets(
		context.Background(),
		fixture.args(),
		&output,
		defaultCredentials(fixture.credentials),
		readerFactory(t, reader, fixture.credentials),
		func(context.Context, string) error {
			t.Fatal("certificate with the wrong public key signed in")
			return nil
		},
	)
	if code != 0 {
		t.Fatalf("runPiSecrets exit = %d, want 0", code)
	}
	requireFileAbsent(t, filepath.Join(fixture.outDir, "device.crt"))
	requireKeptCertificate(t, output.String(), 12, mismatchedCertificate, "candidate public key does not match --key")

	want := make([]secretAccess, 0, 10)
	want = append(want, secretAccess{project: fixture.project, secret: fixture.secret, version: "latest"})
	for version := 11; version >= 3; version-- {
		want = append(want, secretAccess{project: fixture.project, secret: fixture.secret, version: fmt.Sprint(version)})
	}
	requireAccesses(t, reader, want)
}

// TestPiSecretsKeepsTheInstalledCertificate catches each rejection path that
// would overwrite a working installed certificate or report the wrong reason
// for keeping it. TestPiSecretsStagesANewerCertificateThatSignsIn is this
// test's passing control.
func TestPiSecretsKeepsTheInstalledCertificate(t *testing.T) {
	for _, tc := range []struct {
		name      string
		candidate func(t *testing.T, f certificateFixture) []byte
		reason    string
	}{
		{
			name: "not newer",
			candidate: func(t *testing.T, f certificateFixture) []byte {
				return certificateForKey(t, f.ca, f.caKey, f.key, time.Now().Add(12*time.Hour))
			},
			reason: "not newer",
		},
		{
			name: "another key",
			candidate: func(t *testing.T, f certificateFixture) []byte {
				return certificateForKey(t, f.ca, f.caKey, newRSAKey(t), time.Now().Add(72*time.Hour))
			},
			reason: "candidate public key does not match --key",
		},
		{
			name: "fails the sign-in",
			candidate: func(t *testing.T, f certificateFixture) []byte {
				ca, caKey := newUntrustedCertificateAuthority(t)
				return certificateForKey(t, ca, caKey, f.key, time.Now().Add(72*time.Hour))
			},
			reason: "candidate does not sign in",
		},
		{
			name: "not PEM",
			candidate: func(*testing.T, certificateFixture) []byte {
				return []byte("not a PEM certificate")
			},
			reason: "candidate is not exactly one PEM certificate",
		},
		{
			name: "multiple certificates",
			candidate: func(t *testing.T, f certificateFixture) []byte {
				return append(append([]byte(nil), f.candidate...), f.candidate...)
			},
			reason: "candidate is not exactly one PEM certificate",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newCertificateFixture(t)
			candidate := tc.candidate(t, fixture)
			reader := certificateReader(fixture.secret, 1, candidate)
			var output bytes.Buffer

			code := runPiSecrets(
				context.Background(),
				fixture.args(),
				&output,
				defaultCredentials(fixture.credentials),
				readerFactory(t, reader, fixture.credentials),
				productionSignInCheck,
			)
			if code != 0 {
				t.Fatalf("runPiSecrets exit = %d, want 0", code)
			}
			requireFileAbsent(t, filepath.Join(fixture.outDir, "device.crt"))
			requireKeptCertificate(t, output.String(), 1, candidate, tc.reason)
			requireAccesses(t, reader, []secretAccess{{project: fixture.project, secret: fixture.secret, version: "latest"}})
		})
	}
}

func requireKeptCertificate(t *testing.T, output string, version int, candidate []byte, reason string) {
	t.Helper()
	want := fmt.Sprintf("pi-secrets: certificate version %d kept: %s\n", version, reason)
	if output != want {
		t.Errorf("certificate stdout = %q, want %q", output, want)
	}
	if strings.Contains(output, string(candidate)) {
		t.Errorf("certificate stdout contains the candidate value")
	}
}

func secretManagerStatus(code string) error {
	return newSecretManagerError(http.StatusBadRequest, []byte(fmt.Sprintf(`{"error":{"status":%q}}`, code)))
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func newTestSecretManagerReader(transport roundTripFunc) *secretManagerReader {
	return &secretManagerReader{
		token:  func(context.Context) (string, error) { return "access-token", nil },
		client: &http.Client{Transport: transport},
	}
}

// TestSecretManagerReaderAccess catches a reader that sends the wrong access
// request, omits its bearer token, decodes the payload wrongly, or trusts a
// version other than the one in Secret Manager's response name.
func TestSecretManagerReaderAccess(t *testing.T) {
	data := []byte("secret-value")
	crc := crc32.Checksum(data, crc32.MakeTable(crc32.Castagnoli))
	body, err := json.Marshal(map[string]any{
		"name": "projects/example-project/secrets/environment-secret/versions/9",
		"payload": map[string]string{
			"data":       base64.StdEncoding.EncodeToString(data),
			"dataCrc32c": fmt.Sprint(crc),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	reader := newTestSecretManagerReader(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", request.Method)
		}
		if request.URL.String() != secretManagerAccessBase+"/projects/example-project/secrets/environment-secret/versions/latest:access" {
			t.Errorf("URL = %s", request.URL)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer access-token" {
			t.Errorf("Authorization = %q, want bearer token", got)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}, nil
	})

	got, err := reader.Access(context.Background(), "example-project", "environment-secret", "latest")
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 9 || !bytes.Equal(got.Data, data) {
		t.Errorf("Access = %#v, want version 9 and %q", got, data)
	}
}

// TestSecretManagerReaderClassifiesUnavailableVersions catches error parsing
// that fails to continue past disabled or deleted certificate versions.
func TestSecretManagerReaderClassifiesUnavailableVersions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		code   string
	}{
		{name: "disabled", status: http.StatusBadRequest, code: "FAILED_PRECONDITION"},
		{name: "deleted", status: http.StatusNotFound, code: "NOT_FOUND"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(fmt.Sprintf(`{"error":{"status":%q}}`, tc.code))
			reader := newTestSecretManagerReader(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}, nil
			})

			_, err := reader.Access(context.Background(), "example-project", "certificate-secret", "2")
			var apiError *secretManagerAPIError
			if !errors.As(err, &apiError) || apiError.status != tc.status || apiError.code != tc.code {
				t.Fatalf("Access error = %#v, want %d %s secretManagerAPIError", err, tc.status, tc.code)
			}
			if !secretVersionUnavailable(err) {
				t.Errorf("secretVersionUnavailable(%v) = false", err)
			}
		})
	}
}

// TestProductionSignInCheckUsesTheCandidateCertificate catches a sign-in
// check that accepts a certificate from an untrusted CA or does not obtain an
// access token from the temporary credentials file.
func TestProductionSignInCheckUsesTheCandidateCertificate(t *testing.T) {
	fixture := newCertificateFixture(t)
	if reason := certificateSignsIn(context.Background(), fixture.credentials, fixture.candidate, fixture.keyPath, productionSignInCheck); reason != "" {
		t.Fatalf("fixture certificate sign-in reason = %q, want success", reason)
	}
	untrustedCA, untrustedCAKey := newUntrustedCertificateAuthority(t)
	untrustedCandidate := certificateForKey(t, untrustedCA, untrustedCAKey, fixture.key, time.Now().Add(72*time.Hour))
	if reason := certificateSignsIn(context.Background(), fixture.credentials, untrustedCandidate, fixture.keyPath, productionSignInCheck); reason != "candidate does not sign in" {
		t.Fatalf("untrusted certificate sign-in reason = %q, want candidate does not sign in", reason)
	}
}

// TestPiSecretsWithCredentialsNeverLoadsTheConfig catches main or the command
// loading a missing configuration before honoring an explicit --credentials
// path, while retaining the default-credentials control.
func TestPiSecretsWithCredentialsNeverLoadsTheConfig(t *testing.T) {
	for _, tc := range []struct {
		name            string
		args            []string
		defaultFunction func(t *testing.T) func() (string, error)
		wantCredentials string
	}{
		{
			name:            "explicit credentials",
			args:            []string{"--credentials", "override-credentials"},
			wantCredentials: "override-credentials",
			defaultFunction: func(t *testing.T) func() (string, error) {
				return func() (string, error) {
					t.Fatal("default credentials function was called with --credentials")
					return "", nil
				}
			},
		},
		{
			name:            "default credentials",
			wantCredentials: "default-credentials",
			defaultFunction: func(t *testing.T) func() (string, error) {
				calls := 0
				return func() (string, error) {
					calls++
					t.Cleanup(func() {
						if calls != 1 {
							t.Errorf("default credentials calls = %d, want 1", calls)
						}
					})
					return "default-credentials", nil
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := &fakeSecretReader{replies: map[string]secretReply{
				secretReplyKey("environment-secret", "latest"): {value: secretVersion{Version: 1, Data: []byte("ordinary-value")}},
			}}
			outDir := t.TempDir()
			args := []string{"pi-secrets", "--project", "example-project", "--out", outDir, "--env-secret", "SAFE=environment-secret"}
			args = append(args, tc.args...)

			code := runPiSecrets(
				context.Background(),
				args,
				io.Discard,
				tc.defaultFunction(t),
				readerFactory(t, reader, tc.wantCredentials),
				func(context.Context, string) error { return nil },
			)
			if code != 0 {
				t.Fatalf("runPiSecrets exit = %d, want 0", code)
			}
		})
	}
}

// TestPiSecretsTurnsClientCertificatesOff catches a command that inherits an
// unset client-certificate policy instead of disabling it, or overwrites a
// policy deliberately supplied by its caller.
func TestPiSecretsTurnsClientCertificatesOff(t *testing.T) {
	for _, tc := range []struct {
		name      string
		presetSet bool
		preset    string
		want      string
	}{
		{name: "unset", want: "false"},
		{name: "caller preset", presetSet: true, preset: "true", want: "true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.presetSet {
				unsetEnvironment(t, piSecretsClientCertificate)
			} else {
				t.Setenv(piSecretsClientCertificate, tc.preset)
			}

			fixture := newCertificateFixture(t)
			reader := certificateReader(fixture.secret, 1, fixture.candidate)
			var output bytes.Buffer
			code := runPiSecrets(
				context.Background(),
				fixture.args(),
				&output,
				defaultCredentials(fixture.credentials),
				readerFactory(t, reader, fixture.credentials),
				signInWithClientCertificateEnvironment(t, tc.want, nil),
			)
			if code != 0 {
				t.Fatalf("runPiSecrets exit = %d, want 0", code)
			}
			requireCertificateStaged(t, &output, fixture.outDir, 1, fixture.candidate)
		})
	}
}

func unsetEnvironment(t *testing.T, key string) {
	t.Helper()
	old, wasSet := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if wasSet {
			if err := os.Setenv(key, old); err != nil {
				t.Error(err)
			}
			return
		}
		if err := os.Unsetenv(key); err != nil {
			t.Error(err)
		}
	})
}
