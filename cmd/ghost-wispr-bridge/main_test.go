package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// mainTestEnv is a full set of C6's bridge settings, with neutral values.
func mainTestEnv() map[string]string {
	return map[string]string{
		"BRIDGE_PI_ADDRESS":        "100.64.0.7",
		"BRIDGE_PI_NAME":           "pi.example.com",
		"BRIDGE_PI_AUDIENCE":       "https://pi.example.com",
		"BRIDGE_ORG_DOMAINS":       "example.com, example.org",
		"BRIDGE_TESTERS":           "tester@example.com",
		"BRIDGE_CALENDAR_USER":     "bot@meet.example.com",
		"BRIDGE_ATTENDEE_URL":      "http://127.0.0.1:8000",
		"BRIDGE_ATTENDEE_API_KEY":  "api-key",
		"BRIDGE_WEBHOOK_SECRET":    "webhook-secret",
		"BRIDGE_KEY":               "bridge-key",
		"BRIDGE_TLS_CERT":          "-----BEGIN CERTIFICATE-----\nAA==\n-----END CERTIFICATE-----\n",
		"BRIDGE_TLS_KEY_FILE":      "/secrets/tls/key.pem",
		"BRIDGE_AUDIO_PORT":        "9443",
		"BRIDGE_WEBHOOK_PORT":      "9081",
		"BRIDGE_HEALTH_PORT":       "9082",
		"BRIDGE_RUN_SPEC_FILE":     "/run-spec/spec.json",
		"BRIDGE_PROBE_DNS_ADDRESS": "10.126.0.2:53",
	}
}

func mainGetenv(env map[string]string) func(string) string {
	return func(k string) string { return env[k] }
}

func TestSettingsFromEnvReadsEachBridgeSetting(t *testing.T) {
	got, err := settingsFromEnv(mainGetenv(mainTestEnv()))
	if err != nil {
		t.Fatalf("settingsFromEnv: %v", err)
	}
	want := settings{
		PiAddress:       "100.64.0.7",
		PiName:          "pi.example.com",
		PiAudience:      "https://pi.example.com",
		OrgDomains:      []string{"example.com", "example.org"},
		Testers:         []string{"tester@example.com"},
		CalendarUser:    "bot@meet.example.com",
		AttendeeURL:     "http://127.0.0.1:8000",
		AttendeeAPIKey:  "api-key",
		WebhookSecret:   "webhook-secret",
		BridgeKey:       []byte("bridge-key"),
		TLSCert:         "-----BEGIN CERTIFICATE-----\nAA==\n-----END CERTIFICATE-----\n",
		TLSKeyFile:      "/secrets/tls/key.pem",
		AudioPort:       9443,
		WebhookPort:     9081,
		HealthPort:      9082,
		RunSpecFile:     "/run-spec/spec.json",
		ProbeDNSAddress: "10.126.0.2:53",
		MetadataURL:     "http://metadata.google.internal",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("settings:\n got %+v\nwant %+v", got, want)
	}
}

func TestSettingsFromEnvTakesAnEmptyTesterListNoRunSpecAndNoProbeDNS(t *testing.T) {
	env := mainTestEnv()
	delete(env, "BRIDGE_TESTERS")
	delete(env, "BRIDGE_RUN_SPEC_FILE")
	delete(env, "BRIDGE_PROBE_DNS_ADDRESS")
	got, err := settingsFromEnv(mainGetenv(env))
	if err != nil {
		t.Fatalf("settingsFromEnv: %v", err)
	}
	if len(got.Testers) != 0 || got.RunSpecFile != "" || got.ProbeDNSAddress != "" {
		t.Fatalf("testers %q run spec %q probe DNS %q, want none", got.Testers, got.RunSpecFile, got.ProbeDNSAddress)
	}
}

func TestSettingsFromEnvRefusesAMissingOrBadSetting(t *testing.T) {
	for _, tc := range []struct{ key, value string }{
		{"BRIDGE_PI_ADDRESS", ""},
		{"BRIDGE_PI_NAME", ""},
		{"BRIDGE_PI_AUDIENCE", ""},
		{"BRIDGE_ORG_DOMAINS", " , "},
		{"BRIDGE_CALENDAR_USER", ""},
		{"BRIDGE_ATTENDEE_URL", ""},
		{"BRIDGE_ATTENDEE_API_KEY", ""},
		{"BRIDGE_WEBHOOK_SECRET", ""},
		{"BRIDGE_KEY", ""},
		{"BRIDGE_TLS_CERT", ""},
		{"BRIDGE_TLS_KEY_FILE", ""},
		{"BRIDGE_AUDIO_PORT", ""},
		{"BRIDGE_WEBHOOK_PORT", "http"},
		{"BRIDGE_HEALTH_PORT", "70000"},
	} {
		env := mainTestEnv()
		env[tc.key] = tc.value
		_, err := settingsFromEnv(mainGetenv(env))
		if err == nil {
			t.Errorf("%s=%q: settingsFromEnv took it", tc.key, tc.value)
			continue
		}
		if !strings.Contains(err.Error(), tc.key) {
			t.Errorf("%s=%q: error %q does not name the setting", tc.key, tc.value, err)
		}
	}
}

func TestAMissingOrUnknownModePrintsUsageAndExits2(t *testing.T) {
	for _, args := range [][]string{nil, {"call"}, {"bridge", "extra"}} {
		var stderr bytes.Buffer
		code := run(t.Context(), args, mainGetenv(nil), io.Discard, &stderr)
		if code != 2 {
			t.Errorf("args %q: exit %d, want 2", args, code)
		}
		if !strings.Contains(stderr.String(), "usage: ghost-wispr-bridge") {
			t.Errorf("args %q: stderr %q has no usage", args, stderr.String())
		}
	}
}

// C9: in bridge mode a settings error is one marked JSON line on stdout,
// which names each refused setting and why, never its value; stderr is
// empty, and the exit code is still 2.
func TestBridgeModeWithABadSettingExits2(t *testing.T) {
	const sentinel = "zebra-sentinel-5521"
	env := mainTestEnv()
	delete(env, "BRIDGE_ATTENDEE_API_KEY")
	env["BRIDGE_WEBHOOK_PORT"] = sentinel
	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{"bridge"}, mainGetenv(env), &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr %q, want nothing: the settings error is a marked line on stdout", stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("stdout = %q, want one line", stdout.String())
	}
	var line map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &line); err != nil {
		t.Fatalf("the line is not JSON: %s", lines[0])
	}
	want := "SETTINGS-REFUSED BRIDGE_ATTENDEE_API_KEY=unset BRIDGE_WEBHOOK_PORT=malformed"
	if line["severity"] != "ERROR" || line["message"] != want || loglineMark(line, "bridge") != nil {
		t.Fatalf("the line = %s, want a marked ERROR line of the bridge %q (%v)", lines[0], want, loglineMark(line, "bridge"))
	}
	if strings.Contains(stdout.String(), sentinel) {
		t.Fatalf("the line holds the refused value: %s", lines[0])
	}
}

// C9: net/http's own report of a failed TLS handshake, as Attendee's socket
// client makes when it does not trust the certificate, is one marked line
// that names the server and the failure's kind; nothing goes to the standard
// logger, the process's stderr, and the line holds no client address or
// error text.
func TestAServerFailureIsOneMarkedLine(t *testing.T) {
	stderr := botLifeStderr(t)
	logs := &botLogs{}
	certPEM, keyFile, _ := audiosocketCert(t)
	cert, err := loadAudioCert(certPEM, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	sock := newAudioSocket([]byte(audiosocketKey), make(chan audioIn), time.Now)
	go func() { done <- serveAudioSocket(ctx, ln, cert, sock, newLogger(logs, "bridge", slog.LevelDebug)) }()
	defer func() { cancel(); <-done }()

	conn, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{ServerName: "localhost", RootCAs: x509.NewCertPool(), MinVersion: tls.VersionTLS12})
	if err == nil {
		_ = conn.Close()
		t.Fatal("the handshake passed with a client that trusts no certificate")
	}
	clockWaitFor(t, func() bool { return logs.String() != "" || stderr.String() != "" }, "the server's report of the failed handshake")
	time.Sleep(100 * time.Millisecond) // a second report would come by now
	if s := stderr.String(); s != "" {
		t.Fatalf("net/http wrote an unmarked line to stderr: %q", s)
	}
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	var line map[string]any
	if len(lines) != 1 || json.Unmarshal([]byte(lines[0]), &line) != nil {
		t.Fatalf("log = %q, want one JSON line", logs.String())
	}
	if line["message"] != "SERVER-ERROR server=audio_socket kind=tls_handshake" || loglineMark(line, "bridge") != nil {
		t.Fatalf("the line = %s, want the bridge's marked SERVER-ERROR of the audio socket's TLS handshake (%v)", lines[0], loglineMark(line, "bridge"))
	}
	for _, word := range []string{"127.0.0.1", "tls:", "handshake error"} {
		if strings.Contains(lines[0], word) {
			t.Fatalf("the line holds %q: %s", word, lines[0])
		}
	}
}

// mainServe runs serve on a listener of 127.0.0.1 and gives its address and
// a stop that ends serve's context, waits for its return and gives how long
// the return took.
func mainServe(t *testing.T, serve func(ctx context.Context, ln net.Listener) error) (string, func() time.Duration) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- serve(ctx, ln) }()
	return ln.Addr().String(), func() time.Duration {
		t.Helper()
		begin := time.Now()
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("serve: %v", err)
			}
		case <-time.After(clockWaitLimit):
			t.Fatal("the server did not return after its context ended")
		}
		return time.Since(begin)
	}
}

// CR17: the webhook server stops with the service. A webhook whose delivery
// waits on the bridge's loop gets 503 when the service's context ends, so
// Attendee tries it again, and the server's stop does not wait on it.
func TestTheWebhookServerStopsWithTheService(t *testing.T) {
	logs := &botLogs{}
	log := newLogger(logs, "bridge", slog.LevelDebug)
	r, err := newWebhookReceiver(webhookTestSecret, make(chan webhookEvent), log) // nobody takes a webhook
	if err != nil {
		t.Fatal(err)
	}
	addr, stop := mainServe(t, func(ctx context.Context, ln net.Listener) error { return serveWebhooks(ctx, ln, r, log) })
	f := webhookReadFixture(t, "state_change_ended.json")
	answered := make(chan int, 1)
	go func() {
		req, err := http.NewRequest(http.MethodPost, "http://"+addr+webhookPath, strings.NewReader(f.Body))
		if err != nil {
			answered <- -1
			return
		}
		req.Header.Set("X-Webhook-Signature", f.Signature)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			answered <- 0
			return
		}
		_ = resp.Body.Close()
		answered <- resp.StatusCode
	}()
	clockWaitFor(t, func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return len(r.pending) == 1
	}, "the webhook waiting on its delivery")
	if d := stop(); d > 2*time.Second {
		t.Fatalf("the webhook server took %v to stop with a webhook waiting", d)
	}
	select {
	case code := <-answered:
		if code != http.StatusServiceUnavailable {
			t.Fatalf("the waiting webhook got %d, want 503", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiting webhook got no answer after the service stopped")
	}
	if strings.Contains(logs.String(), "SERVER-SHUTDOWN-FAILED") {
		t.Fatalf("the stop failed: %s", logs.String())
	}
}

// C9: each of the bridge's HTTP servers is made by newServer, so net/http's
// own reports are marked lines that name the server and the report's kind
// alone. A handler's panic on the webhook server is one such line and
// nothing on stderr; the health server's report of a handshake holds no
// client address.
func TestEachServerWritesOnlyMarkedLines(t *testing.T) {
	const sentinel = "zebra-sentinel-3301"
	stderr := botLifeStderr(t)
	logs := &botLogs{}
	log := newLogger(logs, "bridge", slog.LevelDebug)
	panics := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(sentinel) })
	addr, stop := mainServe(t, func(ctx context.Context, ln net.Listener) error { return serveWebhooks(ctx, ln, panics, log) })
	if resp, err := http.Post("http://"+addr+webhookPath, "application/json", strings.NewReader("{}")); err == nil {
		_ = resp.Body.Close()
	}
	clockWaitFor(t, func() bool { return logs.String() != "" || stderr.String() != "" }, "the webhook server's report of the panic")
	stop()
	if s := stderr.String(); s != "" {
		t.Fatalf("net/http wrote an unmarked line to stderr: %q", s)
	}
	want := `"message":"SERVER-ERROR server=webhook kind=panic"`
	if got := logs.String(); !strings.Contains(got, want) || strings.Contains(got, sentinel) || strings.Count(got, "\n") != 1 {
		t.Fatalf("log = %s, want one line %s without the panic's text", got, want)
	}

	health := newServer(t.Context(), "health", healthHandler(), log)
	health.ErrorLog.Printf("http: TLS handshake error from 192.0.2.7:4431: EOF")
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if last := lines[len(lines)-1]; !strings.Contains(last, `"message":"SERVER-ERROR server=health kind=tls_handshake"`) || strings.Contains(last, "192.0.2.7") {
		t.Fatalf("the health server's line = %s, want its marked TLS handshake line with no address", last)
	}
	for _, raw := range lines {
		var line map[string]any
		if err := json.Unmarshal([]byte(raw), &line); err != nil || loglineMark(line, "bridge") != nil {
			t.Fatalf("a server's line is not the bridge's marked line: %s (%v)", raw, loglineMark(line, "bridge"))
		}
	}
}

func TestHealthzAnswers200(t *testing.T) {
	srv := httptest.NewServer(healthHandler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz: %d", resp.StatusCode)
	}
	resp, err = http.Get(srv.URL + "/other")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("/other: %d, want 404", resp.StatusCode)
	}
}

func TestServeBridgeRefusesABadAudioCertificate(t *testing.T) {
	s, err := settingsFromEnv(mainGetenv(mainTestEnv()))
	if err != nil {
		t.Fatal(err)
	}
	s.TLSKeyFile = t.TempDir() + "/missing.pem"
	err = serveBridge(t.Context(), &s, newLogger(io.Discard, "bridge", slog.LevelDebug))
	if err == nil || !strings.Contains(err.Error(), "audio socket certificate") {
		t.Fatalf("serveBridge with a bad certificate = %v, want an error naming the audio socket certificate", err)
	}
}
