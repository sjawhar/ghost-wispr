package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/config"
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
		{name: "two final line feeds", value: []byte("control-value\n\n"), marker: "control-value"},
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

// TestPiSecretsEndsWithin30s catches a command with no bound of its own, a
// bound that covers the key reads but not the certificate step, a reader whose
// HTTP request does not carry the command's context, and a bound of 30 s for
// each step instead of one for the whole run. A read or a sign-in that blocks
// waits until its context ends, and the test's context has no deadline, so
// only pi-secrets' own bound can end it. One still blocked after a minute gives
// up and fails the test. Each case runs in a synctest bubble, whose fake clock
// passes the 30 s at once. The files are made outside the bubbles.
func TestPiSecretsEndsWithin30s(t *testing.T) {
	ca, caKey, _ := googleauthtest.NewCA(t)
	key := newRSAKey(t)
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "device.key")
	writeTestFile(t, keyPath, pkcs8KeyPEM(t, key))
	installedPath := filepath.Join(dir, "installed.crt")
	writeTestFile(t, installedPath, certificateForKey(t, ca, caKey, key, time.Now().Add(24*time.Hour)))
	candidate := certificateForKey(t, ca, caKey, key, time.Now().Add(48*time.Hour))
	// The sign-in is a fake, so this file only has to name a certificate source.
	credentialsPath := filepath.Join(dir, "credentials.json")
	writeTestFile(t, credentialsPath, []byte(`{"type":"external_account","credential_source":{"certificate":{"certificate_config_location":"/nonexistent/certificate_config.json"}}}`))
	certificateFlags := []string{"--certificate-secret", "hanging-certificate", "--key", keyPath, "--installed-certificate", installedPath, "--credentials", credentialsPath}

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
			certificate: certificateFlags,
			wantAccesses: []secretAccess{
				{project: "example-project", secret: "hanging-secret", version: "latest"},
				{project: "example-project", secret: "hanging-certificate", version: "latest"},
			},
			wantLine: "pi-secrets: certificate kept: latest certificate cannot be read: DEADLINE_EXCEEDED\n",
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
					err := waitForTheBound(ctx)
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
				requireEndedByTheBound(t, ended)
				if want := "pi-secrets: could not read SAFE from hanging-secret: DEADLINE_EXCEEDED\n"; strings.Count(output.String(), want) != 1 {
					t.Errorf("stdout = %q, want the line %q once", output.String(), want)
				}
				if tc.wantLine != "" && strings.Count(output.String(), tc.wantLine) != 1 {
					t.Errorf("stdout = %q, want the line %q once", output.String(), tc.wantLine)
				}
				requireFileAbsent(t, filepath.Join(outDir, "secrets.env"))
				requireFileAbsent(t, filepath.Join(outDir, "device.crt"))
			})
		})
	}

	// The production reader's HTTP request carries the command's context: a
	// transport that blocks until the request's context ends is ended by the
	// bound, the key read's request and the certificate read's alike.
	t.Run("the production reader's request", func(t *testing.T) {
		outDir := t.TempDir()
		args := append([]string{"pi-secrets", "--project", "example-project", "--out", outDir, "--env-secret", "SAFE=hanging-secret"}, certificateFlags...)
		synctest.Test(t, func(t *testing.T) {
			var paths []string
			var ended []error
			reader := &secretManagerReader{
				token: func(context.Context) (string, error) { return "access-token", nil },
				client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
					paths = append(paths, request.URL.Path)
					err := waitForTheBound(request.Context())
					ended = append(ended, err)
					return nil, err
				})},
			}
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
			if elapsed := time.Since(start); elapsed > 30*time.Second {
				t.Errorf("pi-secrets ran %v, want at most 30s", elapsed)
			}
			if code != 1 {
				t.Errorf("runPiSecrets exit = %d, want 1", code)
			}
			wantPaths := []string{
				"/v1/projects/example-project/secrets/hanging-secret/versions/latest:access",
				"/v1/projects/example-project/secrets/hanging-certificate/versions/latest:access",
			}
			if !reflect.DeepEqual(paths, wantPaths) {
				t.Errorf("requests = %q, want %q", paths, wantPaths)
			}
			requireEndedByTheBound(t, ended)
			want := "pi-secrets: could not read SAFE from hanging-secret: DEADLINE_EXCEEDED\n" +
				"pi-secrets: certificate kept: latest certificate cannot be read: DEADLINE_EXCEEDED\n"
			if output.String() != want {
				t.Errorf("stdout = %q, want %q", output.String(), want)
			}
			requireFileAbsent(t, filepath.Join(outDir, "secrets.env"))
		})
	})

	// One bound covers the whole run: a key read that takes 20 s leaves the
	// sign-in 10 s. With a bound for each step, the sign-in would block until
	// its one-minute guard, and the run would take 80 s.
	t.Run("a slow read then a sign-in that blocks", func(t *testing.T) {
		outDir := t.TempDir()
		args := append([]string{"pi-secrets", "--project", "example-project", "--out", outDir, "--env-secret", "SAFE=slow-secret"}, certificateFlags...)
		synctest.Test(t, func(t *testing.T) {
			reader := &secretManagerReader{
				token: func(context.Context) (string, error) { return "access-token", nil },
				client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
					switch request.URL.Path {
					case "/v1/projects/example-project/secrets/slow-secret/versions/latest:access":
						select {
						case <-time.After(20 * time.Second):
						case <-request.Context().Done():
							return nil, request.Context().Err()
						}
						return secretManagerAnswer("slow-secret", 2, []byte("slow-value")), nil
					case "/v1/projects/example-project/secrets/hanging-certificate/versions/latest:access":
						return secretManagerAnswer("hanging-certificate", 3, candidate), nil
					}
					return nil, fmt.Errorf("unexpected request %s", request.URL.Path)
				})},
			}
			var signInEnded error
			var output bytes.Buffer
			start := time.Now()

			code := runPiSecrets(
				context.Background(),
				args,
				&output,
				defaultCredentials("credential-file"),
				func(context.Context, string) (secretReader, error) { return reader, nil },
				func(ctx context.Context, _ string) error {
					signInEnded = waitForTheBound(ctx)
					return signInEnded
				},
			)
			if elapsed := time.Since(start); elapsed > 30*time.Second {
				t.Errorf("pi-secrets ran %v, want at most 30s", elapsed)
			}
			if code != 0 {
				t.Errorf("runPiSecrets exit = %d, want 0", code)
			}
			if !errors.Is(signInEnded, context.DeadlineExceeded) {
				t.Errorf("the sign-in ended with %v, want pi-secrets' own deadline", signInEnded)
			}
			want := "pi-secrets: SAFE from slow-secret version 2\n" +
				"pi-secrets: certificate version 3 kept: candidate sign-in ran out of time\n"
			if output.String() != want {
				t.Errorf("stdout = %q, want %q", output.String(), want)
			}
			got, err := os.ReadFile(filepath.Join(outDir, "secrets.env"))
			if err != nil || string(got) != "SAFE=slow-value\n" {
				t.Errorf("secrets.env = %q (%v), want SAFE=slow-value", got, err)
			}
			requireFileAbsent(t, filepath.Join(outDir, "device.crt"))
		})
	})
}

// waitForTheBound blocks until ctx ends and returns why, or returns an error
// after a minute: a test bubble's fake clock reaches that guard only when
// nothing ended ctx in time.
func waitForTheBound(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(time.Minute):
		return errors.New("no bound ended the wait within a minute")
	}
}

// requireEndedByTheBound fails t unless each of ended is pi-secrets' own
// deadline.
func requireEndedByTheBound(t *testing.T, ended []error) {
	t.Helper()
	for i, err := range ended {
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("wait %d ended with %v, want pi-secrets' own deadline", i+1, err)
		}
	}
}

// secretManagerAnswer is Secret Manager's answer to a versions access call
// for version of secret, which holds data, with its CRC32C.
func secretManagerAnswer(secret string, version int, data []byte) *http.Response {
	body, err := json.Marshal(map[string]any{
		"name": fmt.Sprintf("projects/123/secrets/%s/versions/%d", secret, version),
		"payload": map[string]string{
			"data":       base64.StdEncoding.EncodeToString(data),
			"dataCrc32c": fmt.Sprint(crc32.Checksum(data, crc32.MakeTable(crc32.Castagnoli))),
		},
	})
	if err != nil {
		panic(err)
	}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}
}

type certificateFixture struct {
	credentials       string
	project           string
	secret            string
	outDir            string
	keyPath           string
	installed         string
	installedNotAfter time.Time
	candidate         []byte
	key               crypto.Signer
	ca                *x509.Certificate
	caKey             *rsa.PrivateKey
}

// newCertificateFixture is a certificate step with an RSA key written as
// PKCS #8, an installed certificate for another key, and a candidate for the
// key that ends a day after the installed one.
func newCertificateFixture(t *testing.T) certificateFixture {
	t.Helper()
	key := newRSAKey(t)
	return newCertificateFixtureForKey(t, key, pkcs8KeyPEM(t, key))
}

// newCertificateFixtureForKey is newCertificateFixture for key, written to
// the key file as keyFile.
func newCertificateFixtureForKey(t *testing.T, key crypto.Signer, keyFile []byte) certificateFixture {
	t.Helper()
	credentials := googleauthtest.X509CredentialFile(t, "sign-in-token")
	ca, caKey, _ := googleauthtest.NewCA(t)
	now := time.Now().UTC().Truncate(time.Second)
	installedNotAfter := now.Add(24 * time.Hour)
	candidate := certificateForKey(t, ca, caKey, key, now.Add(48*time.Hour))
	installed := certificateForKey(t, ca, caKey, newRSAKey(t), installedNotAfter)

	dir := t.TempDir()
	outDir := filepath.Join(dir, "out")
	if err := os.Mkdir(outDir, 0o700); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "device.key")
	writeTestFile(t, keyPath, keyFile)
	installedPath := filepath.Join(dir, "installed.crt")
	writeTestFile(t, installedPath, installed)

	return certificateFixture{
		credentials:       credentials,
		project:           "example-project",
		secret:            "certificate-secret",
		outDir:            outDir,
		keyPath:           keyPath,
		installed:         installedPath,
		installedNotAfter: installedNotAfter,
		candidate:         candidate,
		key:               key,
		ca:                ca,
		caKey:             caKey,
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

// newP256Key is a key on the curve provision.sh --key makes.
func newP256Key(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// certificateForKey is a client certificate with CommonName ghost-wispr, the
// name the token exchange fixture requires, for key's public key, signed by ca.
func certificateForKey(t *testing.T, ca *x509.Certificate, caKey *rsa.PrivateKey, key crypto.Signer, notAfter time.Time) []byte {
	t.Helper()
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(notAfter.UnixNano()),
		Subject:      pkix.Name{CommonName: "ghost-wispr"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}, ca, key.Public(), caKey)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// pkcs8KeyPEM is key as a "PRIVATE KEY" PEM block, the form openssl req
// -newkey writes.
func pkcs8KeyPEM(t *testing.T, key crypto.Signer) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

// p256KeyFile is one form of a P-256 key file.
type p256KeyFile struct {
	name string
	pem  []byte
}

// p256KeyFiles are the forms a P-256 key file takes: PKCS #8, which openssl
// req -newkey ec -pkeyopt ec_paramgen_curve:P-256 writes, and so provision.sh
// --key; SEC 1 ("EC PRIVATE KEY"), which openssl ecparam -name prime256v1
// -genkey -noout writes; and SEC 1 after an "EC PARAMETERS" block that names
// the curve, which openssl ecparam writes without -noout.
func p256KeyFiles(t *testing.T, key *ecdsa.PrivateKey) []p256KeyFile {
	t.Helper()
	sec1, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	sec1PEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: sec1})
	// prime256v1 is the object identifier 1.2.840.10045.3.1.7.
	curve, err := asn1.Marshal(asn1.ObjectIdentifier{1, 2, 840, 10045, 3, 1, 7})
	if err != nil {
		t.Fatal(err)
	}
	parameters := pem.EncodeToMemory(&pem.Block{Type: "EC PARAMETERS", Bytes: curve})
	return []p256KeyFile{
		{name: "PKCS 8", pem: pkcs8KeyPEM(t, key)},
		{name: "SEC 1", pem: sec1PEM},
		{name: "SEC 1 after EC PARAMETERS", pem: append(parameters, sec1PEM...)},
	}
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
// that cannot complete the real X.509 federation sign-in, and a success line
// that does not give the candidate's end date as update.sh reads it.
func TestPiSecretsStagesANewerCertificateThatSignsIn(t *testing.T) {
	fixture := newCertificateFixture(t)
	candidate := certificateForKey(t, fixture.ca, fixture.caKey, fixture.key, time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC))
	reader := certificateReader(fixture.secret, 3, candidate)
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
	requireCertificateStaged(t, &output, fixture.outDir, 3, candidate)
	if want := "pi-secrets: certificate version 3, not after 2030-01-02T03:04:05Z: newer and signs in\n"; output.String() != want {
		t.Errorf("stdout = %q, want %q", output.String(), want)
	}
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
	requireKeptCertificate(t, output.String(), 5, badNewer, "version 4 cannot be read: PERMISSION_DENIED")
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
// more than the latest version and the nine under it, or more than ten
// versions above the installed one. Below latest: latest is version 12 and
// versions 11 to 3 are absent, so an eleventh read, of version 2, fails it.
// Above the installed version 2: latest is disabled, and so are versions 3
// to 12, so an eleventh read, of version 13, fails it.
func TestPiSecretsReadsAtMostTenCertificateVersions(t *testing.T) {
	t.Run("below latest", func(t *testing.T) {
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
	})
	t.Run("above the installed version", func(t *testing.T) {
		fixture := newCertificateFixture(t)
		reader := &fakeSecretReader{replies: map[string]secretReply{
			secretReplyKey(fixture.secret, "latest"): {err: secretManagerStatus("FAILED_PRECONDITION")},
		}}
		for version := 3; version <= 12; version++ {
			reader.replies[secretReplyKey(fixture.secret, fmt.Sprint(version))] = secretReply{err: secretManagerStatus("FAILED_PRECONDITION")}
		}
		var output bytes.Buffer

		code := runPiSecrets(
			context.Background(),
			append(fixture.args(), "--installed-certificate-version", "2"),
			&output,
			defaultCredentials(fixture.credentials),
			readerFactory(t, reader, fixture.credentials),
			func(context.Context, string) error {
				t.Fatal("a disabled version reached the sign-in")
				return nil
			},
		)
		if code != 0 {
			t.Fatalf("runPiSecrets exit = %d, want 0", code)
		}
		requireFileAbsent(t, filepath.Join(fixture.outDir, "device.crt"))
		if want := "pi-secrets: certificate kept: latest certificate cannot be read: FAILED_PRECONDITION; no readable version above 2\n"; output.String() != want {
			t.Errorf("stdout = %q, want %q", output.String(), want)
		}
		want := make([]secretAccess, 0, 11)
		want = append(want, secretAccess{project: fixture.project, secret: fixture.secret, version: "latest"})
		for version := 3; version <= 12; version++ {
			want = append(want, secretAccess{project: fixture.project, secret: fixture.secret, version: fmt.Sprint(version)})
		}
		requireAccesses(t, reader, want)
	})
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
			name: "the same end date",
			candidate: func(t *testing.T, f certificateFixture) []byte {
				return certificateForKey(t, f.ca, f.caKey, f.key, f.installedNotAfter)
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
			candidate: func(_ *testing.T, f certificateFixture) []byte {
				return append(append([]byte(nil), f.candidate...), f.candidate...)
			},
			reason: "candidate is not exactly one PEM certificate",
		},
		{
			name: "text before the PEM",
			candidate: func(_ *testing.T, f certificateFixture) []byte {
				return append([]byte("subject=CN=ghost-wispr\n"), f.candidate...)
			},
			reason: "candidate is not exactly one PEM certificate",
		},
		{
			name: "PEM headers",
			candidate: func(_ *testing.T, f certificateFixture) []byte {
				block, _ := pem.Decode(f.candidate)
				block.Headers = map[string]string{"Comment": "renewed"}
				return pem.EncodeToMemory(block)
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
// check that accepts a certificate from an untrusted CA, does not obtain an
// access token from the temporary credentials file, or cannot present a P-256
// client certificate, the kind provision.sh --key makes.
func TestProductionSignInCheckUsesTheCandidateCertificate(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  crypto.Signer
	}{
		{name: "RSA", key: newRSAKey(t)},
		{name: "P-256", key: newP256Key(t)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newCertificateFixtureForKey(t, tc.key, pkcs8KeyPEM(t, tc.key))
			if reason := certificateSignsIn(context.Background(), fixture.credentials, fixture.candidate, fixture.keyPath, productionSignInCheck); reason != "" {
				t.Fatalf("fixture certificate sign-in reason = %q, want success", reason)
			}
			untrustedCA, untrustedCAKey := newUntrustedCertificateAuthority(t)
			untrustedCandidate := certificateForKey(t, untrustedCA, untrustedCAKey, fixture.key, time.Now().Add(72*time.Hour))
			if reason := certificateSignsIn(context.Background(), fixture.credentials, untrustedCandidate, fixture.keyPath, productionSignInCheck); reason != "candidate does not sign in" {
				t.Fatalf("untrusted certificate sign-in reason = %q, want candidate does not sign in", reason)
			}
		})
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

// fakeSecretManager is Secret Manager's versions access endpoint over HTTPS,
// under the name secretmanager.googleapis.com, with a server certificate from
// the X.509 fixtures' CA. It records each request it gets.
type fakeSecretManager struct {
	mu       sync.Mutex
	versions map[string]secretVersion
	requests []fakeSecretManagerRequest
}

type fakeSecretManagerRequest struct {
	path          string
	authorization string
}

// newFakeSecretManager answers each path of versions, such as
// /v1/projects/example-project/secrets/NAME/versions/latest:access, with its
// version and value, and any other path with NOT_FOUND. It points the
// process's default HTTP transport, which the production reader's client
// uses, at the server until the test ends.
func newFakeSecretManager(t *testing.T, versions map[string]secretVersion) *fakeSecretManager {
	t.Helper()
	manager := &fakeSecretManager{versions: versions}
	ca, caKey, _ := googleauthtest.NewCA(t)
	certificatePEM, keyPEM := googleauthtest.NewCertificate(t, ca, caKey, &pkix.Name{CommonName: "secretmanager.googleapis.com"}, []string{"secretmanager.googleapis.com"}, nil, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth})
	pair, err := tls.X509KeyPair(certificatePEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(manager.serve))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	t.Cleanup(server.Close)

	previous := http.DefaultTransport
	base, ok := previous.(*http.Transport)
	if !ok {
		t.Fatalf("http.DefaultTransport is a %T, not an *http.Transport", previous)
	}
	routed := base.Clone()
	routed.Proxy = nil
	address := server.Listener.Addr().String()
	var dialer net.Dialer
	routed.DialContext = func(ctx context.Context, network, target string) (net.Conn, error) {
		if target == "secretmanager.googleapis.com:443" {
			target = address
		}
		return dialer.DialContext(ctx, network, target)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	routed.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	http.DefaultTransport = routed
	t.Cleanup(func() { http.DefaultTransport = previous })
	return manager
}

func (m *fakeSecretManager) serve(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	m.requests = append(m.requests, fakeSecretManagerRequest{path: r.URL.Path, authorization: r.Header.Get("Authorization")})
	version, ok := m.versions[r.URL.Path]
	m.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodGet || !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"code":404,"message":"not found","status":"NOT_FOUND"}}`)
		return
	}
	answer := secretManagerAnswer("secret", version.Version, version.Data)
	defer func() { _ = answer.Body.Close() }()
	_, _ = io.Copy(w, answer.Body)
}

// take returns the requests the server got since the last take.
func (m *fakeSecretManager) take() []fakeSecretManagerRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	requests := m.requests
	m.requests = nil
	return requests
}

// refusingTokenExchange is a copy of the credential file at path whose token
// exchange answers each request 401.
func refusingTokenExchange(t *testing.T, path string) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"invalid_grant","error_description":"the certificate is refused"}`)
	}))
	t.Cleanup(server.Close)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var credentials map[string]any
	if err := json.Unmarshal(data, &credentials); err != nil {
		t.Fatal(err)
	}
	credentials["token_url"] = server.URL
	refusing, err := json.Marshal(credentials)
	if err != nil {
		t.Fatal(err)
	}
	refusingPath := filepath.Join(t.TempDir(), "credentials.json")
	writeTestFile(t, refusingPath, refusing)
	return refusingPath
}

// TestPiSecretsCommandReadsWithTheProductionReader catches a command entry
// that reads the config before it honours --credentials; a reader that does
// not take its token from the credential file's token exchange, or sends
// another bearer token or another request; and a token failure that writes a
// file, exits 0, prints no reason or still calls Secret Manager. It runs
// runPiSecretsCommand, which main runs, and so the production reader: the
// token exchange is the X.509 fixture's, and Secret Manager a fake HTTPS
// server under its own name.
func TestPiSecretsCommandReadsWithTheProductionReader(t *testing.T) {
	t.Setenv(config.EnvPrefix+"GOOGLE_CREDENTIALS_FILE", "")
	credentials := googleauthtest.X509CredentialFile(t, "production-token")
	const latestPath = "/v1/projects/example-project/secrets/environment-secret/versions/latest:access"
	manager := newFakeSecretManager(t, map[string]secretVersion{latestPath: {Version: 6, Data: []byte("production-value\n")}})
	brokenConfig := filepath.Join(t.TempDir(), "ghost-wispr.yaml")
	writeTestFile(t, brokenConfig, []byte("providers: [unclosed\n"))
	run := func(t *testing.T, credentialFlags ...string) (code int, output, outDir string) {
		t.Helper()
		outDir = t.TempDir()
		var stdout bytes.Buffer
		args := append([]string{"pi-secrets", "--project", "example-project", "--out", outDir, "--env-secret", "SAFE=environment-secret"}, credentialFlags...)
		code = runPiSecretsCommand(context.Background(), args, &stdout, brokenConfig)
		if strings.Contains(stdout.String(), "production-value") {
			t.Errorf("stdout contains the secret value: %q", stdout.String())
		}
		return code, stdout.String(), outDir
	}

	t.Run("--credentials with a config that does not load", func(t *testing.T) {
		code, output, outDir := run(t, "--credentials", credentials)
		if code != 0 {
			t.Fatalf("exit = %d, want 0; stdout %q", code, output)
		}
		if want := "pi-secrets: SAFE from environment-secret version 6\n"; output != want {
			t.Errorf("stdout = %q, want %q", output, want)
		}
		got, err := os.ReadFile(filepath.Join(outDir, "secrets.env"))
		if err != nil || string(got) != "SAFE=production-value\n" {
			t.Errorf("secrets.env = %q (%v), want SAFE=production-value", got, err)
		}
		want := []fakeSecretManagerRequest{{path: latestPath, authorization: "Bearer production-token"}}
		if requests := manager.take(); !reflect.DeepEqual(requests, want) {
			t.Errorf("Secret Manager requests = %#v, want %#v", requests, want)
		}
	})
	t.Run("the same config without --credentials", func(t *testing.T) {
		code, output, outDir := run(t)
		if code != 1 {
			t.Errorf("exit = %d, want 1", code)
		}
		if want := "pi-secrets: could not read the config's google_credentials_file; pass --credentials\n"; output != want {
			t.Errorf("stdout = %q, want %q", output, want)
		}
		requireFileAbsent(t, filepath.Join(outDir, "secrets.env"))
		if requests := manager.take(); len(requests) != 0 {
			t.Errorf("Secret Manager requests = %#v, want none", requests)
		}
	})
	t.Run("a token exchange that refuses", func(t *testing.T) {
		code, output, outDir := run(t, "--credentials", refusingTokenExchange(t, credentials))
		if code != 1 {
			t.Errorf("exit = %d, want 1", code)
		}
		if want := "pi-secrets: could not read SAFE from environment-secret: sign-in failed\n"; output != want {
			t.Errorf("stdout = %q, want %q", output, want)
		}
		requireFileAbsent(t, filepath.Join(outDir, "secrets.env"))
		if requests := manager.take(); len(requests) != 0 {
			t.Errorf("Secret Manager requests = %#v, want none", requests)
		}
	})
}

// TestPiSecretsCommandRefusesAConfigWithNoCredentialsFile catches a command
// that, with no --credentials and no google_credentials_file in the config,
// signs in with Application Default Credentials, which on the Pi are whatever
// its environment points at. GOOGLE_APPLICATION_CREDENTIALS names a credential
// file that works here, so a fallback would read the secret.
func TestPiSecretsCommandRefusesAConfigWithNoCredentialsFile(t *testing.T) {
	t.Setenv(config.EnvPrefix+"GOOGLE_CREDENTIALS_FILE", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLOUDSDK_CONFIG", t.TempDir())
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", googleauthtest.X509CredentialFile(t, "default-token"))
	manager := newFakeSecretManager(t, map[string]secretVersion{
		"/v1/projects/example-project/secrets/environment-secret/versions/latest:access": {Version: 1, Data: []byte("default-value")},
	})
	configPath := filepath.Join(t.TempDir(), "ghost-wispr.yaml")
	writeTestFile(t, configPath, []byte("silence_timeout: 30s\n"))
	outDir := t.TempDir()
	var output bytes.Buffer

	code := runPiSecretsCommand(
		context.Background(),
		[]string{"pi-secrets", "--project", "example-project", "--out", outDir, "--env-secret", "SAFE=environment-secret"},
		&output,
		configPath,
	)
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if want := "pi-secrets: the config names no google_credentials_file; pass --credentials\n"; output.String() != want {
		t.Errorf("stdout = %q, want %q", output.String(), want)
	}
	requireFileAbsent(t, filepath.Join(outDir, "secrets.env"))
	if requests := manager.take(); len(requests) != 0 {
		t.Errorf("Secret Manager requests = %#v, want none", requests)
	}
}

// TestPiSecretsWithAP256Key catches a key reader or a sign-in that handles RSA
// keys alone, or only one form of key file: provision.sh --key makes a P-256
// key. In each form, the key's own certificate is staged through the real
// X.509 sign-in, and another key's certificate is kept.
func TestPiSecretsWithAP256Key(t *testing.T) {
	key := newP256Key(t)
	for _, file := range p256KeyFiles(t, key) {
		t.Run(file.name, func(t *testing.T) {
			t.Run("its own certificate", func(t *testing.T) {
				fixture := newCertificateFixtureForKey(t, key, file.pem)
				reader := certificateReader(fixture.secret, 3, fixture.candidate)
				var output bytes.Buffer
				code := runPiSecrets(context.Background(), fixture.args(), &output, defaultCredentials(fixture.credentials), readerFactory(t, reader, fixture.credentials), productionSignInCheck)
				if code != 0 {
					t.Fatalf("runPiSecrets exit = %d, want 0", code)
				}
				requireCertificateStaged(t, &output, fixture.outDir, 3, fixture.candidate)
			})
			t.Run("another key's certificate", func(t *testing.T) {
				fixture := newCertificateFixtureForKey(t, key, file.pem)
				other := certificateForKey(t, fixture.ca, fixture.caKey, newP256Key(t), time.Now().Add(72*time.Hour))
				reader := certificateReader(fixture.secret, 3, other)
				var output bytes.Buffer
				code := runPiSecrets(context.Background(), fixture.args(), &output, defaultCredentials(fixture.credentials), readerFactory(t, reader, fixture.credentials), productionSignInCheck)
				if code != 0 {
					t.Fatalf("runPiSecrets exit = %d, want 0", code)
				}
				requireFileAbsent(t, filepath.Join(fixture.outDir, "device.crt"))
				requireKeptCertificate(t, output.String(), 3, other, "candidate public key does not match --key")
			})
		})
	}
}

// TestPiSecretsReadsAboveTheInstalledVersion catches a certificate step that
// gives up when latest is disabled although an enabled version newer than the
// installed one exists, so the Pi would never renew again; one that walks up
// without --installed-certificate-version, past a NOT_FOUND, or when latest
// was refused rather than disabled or absent; one that stages the newest
// version above the installed one rather than the newest that passes every
// check; and one that walks up when latest can be read.
func TestPiSecretsReadsAboveTheInstalledVersion(t *testing.T) {
	run := func(t *testing.T, f *certificateFixture, flags []string, replies map[string]secretReply) (*fakeSecretReader, string) {
		t.Helper()
		reader := &fakeSecretReader{replies: replies}
		var output bytes.Buffer
		code := runPiSecrets(context.Background(), append(f.args(), flags...), &output, defaultCredentials(f.credentials), readerFactory(t, reader, f.credentials), productionSignInCheck)
		if code != 0 {
			t.Fatalf("runPiSecrets exit = %d, want 0; stdout %q", code, output.String())
		}
		return reader, output.String()
	}
	accesses := func(f *certificateFixture, versions ...string) []secretAccess {
		want := make([]secretAccess, 0, len(versions))
		for _, version := range versions {
			want = append(want, secretAccess{project: f.project, secret: f.secret, version: version})
		}
		return want
	}
	disabled := secretReply{err: secretManagerStatus("FAILED_PRECONDITION")}
	absent := secretReply{err: secretManagerStatus("NOT_FOUND")}
	refused := secretReply{err: newSecretManagerError(http.StatusForbidden, []byte(`{"error":{"code":403,"status":"PERMISSION_DENIED"}}`))}
	installedFive := []string{"--installed-certificate-version", "5"}

	t.Run("latest disabled, enabled versions above the installed one", func(t *testing.T) {
		f := newCertificateFixture(t)
		older := certificateForKey(t, f.ca, f.caKey, f.key, time.Now().Add(60*time.Hour))
		newer := certificateForKey(t, f.ca, f.caKey, f.key, time.Now().Add(66*time.Hour))
		untrustedCA, untrustedCAKey := newUntrustedCertificateAuthority(t)
		newest := certificateForKey(t, untrustedCA, untrustedCAKey, f.key, time.Now().Add(72*time.Hour))
		reader, output := run(t, &f, installedFive, map[string]secretReply{
			secretReplyKey(f.secret, "latest"): disabled,
			secretReplyKey(f.secret, "6"):      {value: secretVersion{Version: 6, Data: older}},
			secretReplyKey(f.secret, "7"):      {value: secretVersion{Version: 7, Data: newer}},
			secretReplyKey(f.secret, "8"):      {value: secretVersion{Version: 8, Data: newest}},
			secretReplyKey(f.secret, "9"):      disabled,
			secretReplyKey(f.secret, "10"):     absent,
		})
		requireCertificateStaged(t, bytes.NewBufferString(output), f.outDir, 7, newer)
		requireAccesses(t, reader, accesses(&f, "latest", "6", "7", "8", "9", "10"))
	})
	t.Run("latest disabled, a refused version above the installed one", func(t *testing.T) {
		f := newCertificateFixture(t)
		older := certificateForKey(t, f.ca, f.caKey, f.key, time.Now().Add(60*time.Hour))
		reader, output := run(t, &f, installedFive, map[string]secretReply{
			secretReplyKey(f.secret, "latest"): disabled,
			secretReplyKey(f.secret, "6"):      {value: secretVersion{Version: 6, Data: older}},
			secretReplyKey(f.secret, "7"):      refused,
		})
		requireCertificateStaged(t, bytes.NewBufferString(output), f.outDir, 6, older)
		requireAccesses(t, reader, accesses(&f, "latest", "6", "7"))
	})
	t.Run("latest disabled, no version above the installed one passes", func(t *testing.T) {
		f := newCertificateFixture(t)
		notNewer := certificateForKey(t, f.ca, f.caKey, f.key, time.Now().Add(12*time.Hour))
		reader, output := run(t, &f, installedFive, map[string]secretReply{
			secretReplyKey(f.secret, "latest"): disabled,
			secretReplyKey(f.secret, "6"):      {value: secretVersion{Version: 6, Data: notNewer}},
			secretReplyKey(f.secret, "7"):      absent,
		})
		requireFileAbsent(t, filepath.Join(f.outDir, "device.crt"))
		requireKeptCertificate(t, output, 6, notNewer, "not newer")
		requireAccesses(t, reader, accesses(&f, "latest", "6", "7"))
	})
	t.Run("latest disabled, nothing above the installed one", func(t *testing.T) {
		f := newCertificateFixture(t)
		reader, output := run(t, &f, installedFive, map[string]secretReply{
			secretReplyKey(f.secret, "latest"): disabled,
			secretReplyKey(f.secret, "6"):      absent,
		})
		requireFileAbsent(t, filepath.Join(f.outDir, "device.crt"))
		if want := "pi-secrets: certificate kept: latest certificate cannot be read: FAILED_PRECONDITION; no readable version above 5\n"; output != want {
			t.Errorf("stdout = %q, want %q", output, want)
		}
		requireAccesses(t, reader, accesses(&f, "latest", "6"))
	})
	t.Run("latest disabled, no --installed-certificate-version", func(t *testing.T) {
		f := newCertificateFixture(t)
		reader, output := run(t, &f, nil, map[string]secretReply{
			secretReplyKey(f.secret, "latest"): disabled,
		})
		requireFileAbsent(t, filepath.Join(f.outDir, "device.crt"))
		if want := "pi-secrets: certificate kept: latest certificate cannot be read: FAILED_PRECONDITION\n"; output != want {
			t.Errorf("stdout = %q, want %q", output, want)
		}
		requireAccesses(t, reader, accesses(&f, "latest"))
	})
	t.Run("latest refused", func(t *testing.T) {
		f := newCertificateFixture(t)
		reader, output := run(t, &f, installedFive, map[string]secretReply{
			secretReplyKey(f.secret, "latest"): refused,
		})
		requireFileAbsent(t, filepath.Join(f.outDir, "device.crt"))
		if want := "pi-secrets: certificate kept: latest certificate cannot be read: PERMISSION_DENIED\n"; output != want {
			t.Errorf("stdout = %q, want %q", output, want)
		}
		requireAccesses(t, reader, accesses(&f, "latest"))
	})
	t.Run("latest readable", func(t *testing.T) {
		f := newCertificateFixture(t)
		reader, output := run(t, &f, installedFive, map[string]secretReply{
			secretReplyKey(f.secret, "latest"): {value: secretVersion{Version: 7, Data: f.candidate}},
		})
		requireCertificateStaged(t, bytes.NewBufferString(output), f.outDir, 7, f.candidate)
		requireAccesses(t, reader, accesses(&f, "latest"))
	})
}

// requireDirectoryHolds fails t unless dir holds exactly names.
func requireDirectoryHolds(t *testing.T, dir string, names ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(entries))
	for _, entry := range entries {
		got = append(got, entry.Name())
	}
	if !slices.Equal(got, names) {
		t.Errorf("%s holds %q, want %q", dir, got, names)
	}
}

// directoryAt makes a directory at path that holds one file, so no file can
// be renamed over it.
func directoryAt(t *testing.T, path string) {
	t.Helper()
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(path, "kept"), []byte("kept"))
}

// TestPiSecretsWritesNoPartialSecretsFile catches a write of secrets.env that
// exits 0, prints a value, or leaves a partial or temporary file, when the
// file cannot be created or cannot be renamed into place.
func TestPiSecretsWritesNoPartialSecretsFile(t *testing.T) {
	for _, tc := range []struct {
		name   string
		outDir func(t *testing.T) string
		check  func(t *testing.T, outDir string)
	}{
		{
			name:   "the directory is missing",
			outDir: func(t *testing.T) string { return filepath.Join(t.TempDir(), "missing") },
			check:  func(t *testing.T, outDir string) { requireFileAbsent(t, outDir) },
		},
		{
			name: "the rename fails",
			outDir: func(t *testing.T) string {
				dir := t.TempDir()
				directoryAt(t, filepath.Join(dir, "secrets.env"))
				return dir
			},
			check: func(t *testing.T, outDir string) {
				requireDirectoryHolds(t, outDir, "secrets.env")
				requireDirectoryHolds(t, filepath.Join(outDir, "secrets.env"), "kept")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			outDir := tc.outDir(t)
			reader := &fakeSecretReader{replies: map[string]secretReply{
				secretReplyKey("environment-secret", "latest"): {value: secretVersion{Version: 1, Data: []byte("partial-value")}},
			}}
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
				t.Errorf("exit = %d, want 1", code)
			}
			if want := "pi-secrets: could not write secrets.env\n"; output.String() != want {
				t.Errorf("stdout = %q, want %q", output.String(), want)
			}
			tc.check(t, outDir)
		})
	}
}

// TestPiSecretsKeepsTheSecretsWhenTheCertificateCannotBeStaged catches a
// staging failure of device.crt that changes the exit code or the secrets.env
// result, leaves a temporary file, or reports another version's reason.
func TestPiSecretsKeepsTheSecretsWhenTheCertificateCannotBeStaged(t *testing.T) {
	fixture := newCertificateFixture(t)
	directoryAt(t, filepath.Join(fixture.outDir, "device.crt"))
	reader := &fakeSecretReader{replies: map[string]secretReply{
		secretReplyKey("environment-secret", "latest"): {value: secretVersion{Version: 1, Data: []byte("staging-value")}},
		secretReplyKey(fixture.secret, "latest"):       {value: secretVersion{Version: 3, Data: fixture.candidate}},
	}}
	var output bytes.Buffer
	code := runPiSecrets(
		context.Background(),
		append(fixture.args(), "--env-secret", "SAFE=environment-secret"),
		&output,
		defaultCredentials(fixture.credentials),
		readerFactory(t, reader, fixture.credentials),
		productionSignInCheck,
	)
	if code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
	want := "pi-secrets: SAFE from environment-secret version 1\n" +
		"pi-secrets: certificate version 3 kept: version 3 could not be staged\n"
	if output.String() != want {
		t.Errorf("stdout = %q, want %q", output.String(), want)
	}
	got, err := os.ReadFile(filepath.Join(fixture.outDir, "secrets.env"))
	if err != nil || string(got) != "SAFE=staging-value\n" {
		t.Errorf("secrets.env = %q (%v), want SAFE=staging-value", got, err)
	}
	requireDirectoryHolds(t, fixture.outDir, "device.crt", "secrets.env")
	requireDirectoryHolds(t, filepath.Join(fixture.outDir, "device.crt"), "kept")
}

// TestPiSecretsRefusesAValueSystemdWouldMisread catches a value that
// systemd's EnvironmentFile= parser, or the recorder's own .env reader, would
// read as another value: a backslash escapes the next character and one at
// the end joins the next line to the value, a leading quote starts a quoted
// value, and whitespace at either end is dropped. A value with +, / and =, as
// a base64 token holds, is the control.
func TestPiSecretsRefusesAValueSystemdWouldMisread(t *testing.T) {
	t.Run("a final backslash before the next variable", func(t *testing.T) {
		reader := &fakeSecretReader{replies: map[string]secretReply{
			secretReplyKey("first-secret", "latest"):  {value: secretVersion{Version: 1, Data: []byte("first-value\\")}},
			secretReplyKey("second-secret", "latest"): {value: secretVersion{Version: 1, Data: []byte("second-value")}},
		}}
		outDir := t.TempDir()
		var output bytes.Buffer
		code := runPiSecrets(
			context.Background(),
			[]string{"pi-secrets", "--project", "example-project", "--out", outDir, "--env-secret", "FIRST=first-secret", "--env-secret", "SECOND=second-secret"},
			&output,
			defaultCredentials("credential-file"),
			readerFactory(t, reader, "credential-file"),
			func(context.Context, string) error { return nil },
		)
		if code != 1 {
			t.Errorf("exit = %d, want 1", code)
		}
		requireFileAbsent(t, filepath.Join(outDir, "secrets.env"))
		for _, value := range []string{"first-value", "second-value"} {
			if strings.Contains(output.String(), value) {
				t.Errorf("stdout contains %q: %q", value, output.String())
			}
		}
	})
	for _, tc := range []struct {
		name   string
		value  string
		marker string
	}{
		{name: "a backslash inside", value: `escaped\nvalue`, marker: "escaped"},
		{name: "a leading single quote", value: "'quoted-value'", marker: "quoted-value"},
		{name: "a leading double quote", value: `"quoted-value"`, marker: "quoted-value"},
		{name: "a leading space", value: " spaced-value", marker: "spaced-value"},
		{name: "a trailing space", value: "spaced-value ", marker: "spaced-value"},
		{name: "a leading tab", value: "\tspaced-value", marker: "spaced-value"},
		{name: "a trailing tab", value: "spaced-value\t", marker: "spaced-value"},
		{name: "a trailing no-break space", value: "spaced-value\u00a0", marker: "spaced-value"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireEnvironmentValueRefused(t, []byte(tc.value), tc.marker)
		})
	}
	t.Run("plus, slash and equals", func(t *testing.T) {
		requireEnvironmentValueAccepted(t, []byte("abc+/def=="), "SAFE=abc+/def==\n", "abc+/def")
	})
}

// TestPiSecretsRefusesMalformedArguments catches an argument error that exits
// 0, or 1 as a failed read does, rather than 2, or that loads the config,
// reads a secret or writes a file first.
func TestPiSecretsRefusesMalformedArguments(t *testing.T) {
	outDir := t.TempDir()
	with := func(extra ...string) []string {
		return append([]string{"pi-secrets", "--project", "example-project", "--out", outDir}, extra...)
	}
	certificate := []string{"--certificate-secret", "certificate-secret", "--key", "device.key", "--installed-certificate", "installed.crt"}
	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "a lowercase variable", args: with("--env-secret", "lower=environment-secret")},
		{name: "a variable that starts with a digit", args: with("--env-secret", "1BAD=environment-secret")},
		{name: "a variable with a dash", args: with("--env-secret", "BAD-NAME=environment-secret")},
		{name: "no secret name", args: with("--env-secret", "SAFE=")},
		{name: "no equals sign", args: with("--env-secret", "SAFE")},
		{name: "no --out", args: []string{"pi-secrets", "--project", "example-project", "--env-secret", "SAFE=environment-secret"}},
		{name: "no --project", args: []string{"pi-secrets", "--out", outDir, "--env-secret", "SAFE=environment-secret"}},
		{name: "only --certificate-secret", args: with("--certificate-secret", "certificate-secret")},
		{name: "--key and --installed-certificate alone", args: with("--key", "device.key", "--installed-certificate", "installed.crt")},
		{name: "--installed-certificate-version alone", args: with("--env-secret", "SAFE=environment-secret", "--installed-certificate-version", "5")},
		{name: "--installed-certificate-version 0", args: with(append(certificate, "--installed-certificate-version", "0")...)},
		{name: "--installed-certificate-version not a number", args: with(append(certificate, "--installed-certificate-version", "five")...)},
		{name: "an empty --credentials", args: with("--env-secret", "SAFE=environment-secret", "--credentials", "")},
		{name: "an unknown flag", args: with("--env-secret", "SAFE=environment-secret", "--verbose")},
		{name: "a positional argument", args: with("--env-secret", "SAFE=environment-secret", "extra")},
		{name: "nothing to read", args: with()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			code := runPiSecrets(
				context.Background(),
				tc.args,
				&output,
				func() (string, error) {
					t.Error("an argument error loaded the config")
					return "credential-file", nil
				},
				func(context.Context, string) (secretReader, error) {
					t.Error("an argument error made a Secret Manager reader")
					return &fakeSecretReader{}, nil
				},
				func(context.Context, string) error {
					t.Error("an argument error signed in")
					return nil
				},
			)
			if code != 2 {
				t.Errorf("exit = %d, want 2", code)
			}
			if line := output.String(); !strings.HasPrefix(line, "pi-secrets: ") || strings.Count(line, "\n") != 1 {
				t.Errorf("stdout = %q, want one line that starts pi-secrets: ", line)
			}
			requireDirectoryHolds(t, outDir)
		})
	}
}

// TestPiSecretsSignInReplacesTheDefaultCertificateConfig catches a sign-in
// copy of the credential file that keeps use_default_certificate_config beside
// the certificate_config_location it sets. The token exchange refuses a
// certificate source that names both, so a Pi whose credential file uses
// gcloud's default certificate config would never sign in with a renewal.
func TestPiSecretsSignInReplacesTheDefaultCertificateConfig(t *testing.T) {
	fixture := newCertificateFixture(t)
	data, err := os.ReadFile(fixture.credentials)
	if err != nil {
		t.Fatal(err)
	}
	var credentials map[string]any
	if err := json.Unmarshal(data, &credentials); err != nil {
		t.Fatal(err)
	}
	source, ok := credentials["credential_source"].(map[string]any)
	if !ok {
		t.Fatalf("the fixture's credential file has no credential_source: %s", data)
	}
	source["certificate"] = map[string]any{"use_default_certificate_config": true}
	defaultConfig, err := json.Marshal(credentials)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "credentials.json")
	writeTestFile(t, path, defaultConfig)
	reader := certificateReader(fixture.secret, 3, fixture.candidate)
	var output bytes.Buffer

	code := runPiSecrets(context.Background(), fixture.args(), &output, defaultCredentials(path), readerFactory(t, reader, path), productionSignInCheck)
	if code != 0 {
		t.Fatalf("runPiSecrets exit = %d, want 0", code)
	}
	requireCertificateStaged(t, &output, fixture.outDir, 3, fixture.candidate)
}

// TestPiSecretsNamesWhyAReadFailed catches a failed key read that prints no
// reason, or prints the error's own text, which can quote Secret Manager's
// answer or a request: only a Secret Manager status code, an HTTP status, or
// a fixed phrase may follow the secret's name.
func TestPiSecretsNamesWhyAReadFailed(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{name: "refused", err: newSecretManagerError(http.StatusForbidden, []byte(`{"error":{"code":403,"message":"Permission denied on secret environment-secret","status":"PERMISSION_DENIED"}}`)), want: "PERMISSION_DENIED"},
		{name: "absent", err: newSecretManagerError(http.StatusNotFound, []byte(`{"error":{"status":"NOT_FOUND"}}`)), want: "NOT_FOUND"},
		{name: "an answer with no status", err: newSecretManagerError(http.StatusBadGateway, []byte("<html>bad gateway</html>")), want: "HTTP 502"},
		{name: "a status that is no code", err: newSecretManagerError(http.StatusInternalServerError, []byte(`{"error":{"status":"see https://example.test/status"}}`)), want: "HTTP 500"},
		{name: "past the bound", err: fmt.Errorf("access: %w", context.DeadlineExceeded), want: "DEADLINE_EXCEEDED"},
		{name: "another failure", err: errors.New("dial tcp: lookup secretmanager.googleapis.com: no such host"), want: "request failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := &fakeSecretReader{replies: map[string]secretReply{
				secretReplyKey("environment-secret", "latest"): {err: tc.err},
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
				t.Errorf("exit = %d, want 1", code)
			}
			if want := "pi-secrets: could not read SAFE from environment-secret: " + tc.want + "\n"; output.String() != want {
				t.Errorf("stdout = %q, want %q", output.String(), want)
			}
			requireFileAbsent(t, filepath.Join(outDir, "secrets.env"))
		})
	}
}

// TestTheUpdateScriptReadsTheCertificateLine catches a success line that
// update.sh's CERTIFICATE_PATTERN does not read, which would leave every
// staged certificate uninstalled, and a keep line that the pattern reads as a
// success. It reads the pattern from deploy/pi/update.sh itself.
func TestTheUpdateScriptReadsTheCertificateLine(t *testing.T) {
	script, err := os.ReadFile(filepath.Join("..", "..", "deploy", "pi", "update.sh"))
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`(?m)^readonly CERTIFICATE_PATTERN='([^']+)'$`).FindSubmatch(script)
	if match == nil {
		t.Fatal("deploy/pi/update.sh sets no CERTIFICATE_PATTERN the test can read")
	}
	pattern, err := regexp.Compile(string(match[1]))
	if err != nil {
		t.Fatalf("CERTIFICATE_PATTERN %q: %v", match[1], err)
	}

	fixture := newCertificateFixture(t)
	candidate := certificateForKey(t, fixture.ca, fixture.caKey, fixture.key, time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC))
	reader := certificateReader(fixture.secret, 3, candidate)
	var output bytes.Buffer
	if code := runPiSecrets(context.Background(), fixture.args(), &output, defaultCredentials(fixture.credentials), readerFactory(t, reader, fixture.credentials), productionSignInCheck); code != 0 {
		t.Fatalf("runPiSecrets exit = %d, want 0", code)
	}
	groups := pattern.FindStringSubmatch(strings.TrimSuffix(output.String(), "\n"))
	if len(groups) != 3 || groups[1] != "3" || groups[2] != "2030-01-02T03:04:05Z" {
		t.Errorf("CERTIFICATE_PATTERN %q on %q gives %q, want the version 3 and the end 2030-01-02T03:04:05Z", match[1], output.String(), groups)
	}
	if keep := "pi-secrets: certificate version 3 kept: not newer"; pattern.MatchString(keep) {
		t.Errorf("CERTIFICATE_PATTERN %q reads the keep line %q as a staged certificate", match[1], keep)
	}
}
