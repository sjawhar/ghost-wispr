// Command ghost-wispr-bridge runs in the cloud call instance beside Attendee
// (mode bridge): it holds Attendee's sockets, webhooks and API key, makes the
// bot of each call's event, and links the call to the Pi.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	stdlog "log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	calendarapi "google.golang.org/api/calendar/v3"

	"github.com/sjawhar/ghost-wispr/internal/calendar"
)

const usage = `usage: ghost-wispr-bridge <mode>

modes:
  bridge   hold Attendee's bot for each call's event and link the call to the Pi
  watch    keep the call pool's instance count by the bot user's calendar
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run dispatches on the mode and gives the exit code: 2 for a usage or
// settings error, 1 for a failure after the start.
func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		_, _ = fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "bridge":
		return bridgeMode(ctx, getenv, stdout)
	case "watch":
		return watchMode(ctx, getenv, stdout, stderr)
	default:
		_, _ = fmt.Fprint(stderr, usage)
		return 2
	}
}

// bridgeMode runs the bridge. Every line it writes, a settings error too, is
// one marked JSON line on stdout (C9).
func bridgeMode(ctx context.Context, getenv func(string) string, stdout io.Writer) int {
	log := newLogger(stdout, "bridge", slog.LevelInfo)
	s, err := settingsFromEnv(getenv)
	if err != nil {
		log.Error("SETTINGS-REFUSED " + refusedSettings(err))
		return 2
	}
	if err := serveBridge(ctx, &s, log); err != nil {
		log.Error("BRIDGE-FAILED", "part", failedPart(err), "error", errorClass(err))
		return 1
	}
	return 0
}

// partError is a failure of one part of the bridge: main logs the part's
// fixed name and the error's class alone.
type partError struct {
	part string
	err  error
}

func (e *partError) Error() string { return e.part + ": " + e.err.Error() }
func (e *partError) Unwrap() error { return e.err }

// failedPart names the part of the bridge that failed with err.
func failedPart(err error) string {
	var pe *partError
	if errors.As(err, &pe) {
		return pe.part
	}
	return "bridge"
}

// audioQueue is the room for Attendee's audio messages between the audio
// socket and runBridge's loop: about 2 s of the call mix in 10 ms messages.
const audioQueue = 256

// serveBridge wires the bridge to Attendee, the calendar and its servers
// (health, webhooks, audio socket), and runs them all until ctx ends or one
// fails.
func serveBridge(ctx context.Context, s *settings, log *slog.Logger) error {
	cert, err := loadAudioCert(s.TLSCert, s.TLSKeyFile)
	if err != nil {
		return &partError{"audio socket certificate", err}
	}
	webhooks := make(chan webhookEvent)
	receiver, err := newWebhookReceiver(s.WebhookSecret, webhooks, log)
	if err != nil {
		return &partError{"webhook receiver", err}
	}
	svc, err := delegatedCalendar(ctx, calendarapi.CalendarEventsScope, s.CalendarUser)
	if err != nil {
		return &partError{"calendar", err}
	}
	var lns []net.Listener
	for _, l := range []struct {
		part string
		port int
	}{{"health port", s.HealthPort}, {"webhook port", s.WebhookPort}, {"audio socket port", s.AudioPort}} {
		ln, err := net.Listen("tcp", net.JoinHostPort("", strconv.Itoa(l.port)))
		if err != nil {
			for _, open := range lns {
				_ = open.Close()
			}
			return &partError{l.part, err}
		}
		lns = append(lns, ln)
	}
	clk := realClock{}
	audio := make(chan audioIn, audioQueue)
	sock := newAudioSocket(s.BridgeKey, audio, clk.Now)
	deps := &bridgeDeps{
		API:      newAttendeeClient(s.AttendeeURL, s.AttendeeAPIKey),
		Link:     nil, // the release build has no link until T4.4's client
		Calendar: calendar.NewReader(svc, calendar.Filter{OrgDomains: s.OrgDomains, Testers: s.Testers, BotUser: s.CalendarUser}),
		Clock:    clk,
		Log:      log,
		Webhooks: webhooks,
		Audio:    audio,
		Sockets:  sock,
	}
	return runAll(ctx,
		func(ctx context.Context) error { return serveHealth(ctx, lns[0], log) },
		func(ctx context.Context) error { return serveWebhooks(ctx, lns[1], receiver, log) },
		func(ctx context.Context) error { return serveAudioSocket(ctx, lns[2], cert, sock, log) },
		func(ctx context.Context) error { return startBridge(ctx, s, deps) },
	)
}

// startBridge logs BRIDGE-START, applies the test pool's faults, then runs
// the bridge until ctx ends. A run spec that applyFaults refuses makes no bot
// and no link; the health server stays up, so Cloud Run does not restart the
// container in a loop.
func startBridge(ctx context.Context, s *settings, deps *bridgeDeps) error {
	logBridgeStart(ctx, s.MetadataURL, deps.Log)
	stop, err := applyFaults(ctx, *s, deps)
	if err != nil {
		return &partError{"faults", err}
	}
	if stop {
		<-ctx.Done()
		return nil
	}
	return runBridge(ctx, *s, *deps)
}

// logBridgeStart logs BRIDGE-START with this instance's id once, at the
// bridge's start and before its run spec is read, so a bridge that refuses
// its run spec names its instance too (spec, "The call bridge";
// c-cloudrun-first tells a container's restart from the instance's by it).
func logBridgeStart(ctx context.Context, metadataURL string, log *slog.Logger) {
	log.Info("BRIDGE-START instance=" + instanceID(ctx, metadataURL, log))
}

// instanceID reads Cloud Run's id of this instance from the metadata server
// at metadataURL; "unknown" when it is not set or the read fails.
func instanceID(ctx context.Context, metadataURL string, log *slog.Logger) string {
	if metadataURL == "" {
		return "unknown"
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metadataURL+"/computeMetadata/v1/instance/id", http.NoBody)
	if err != nil {
		return "unknown"
	}
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Warn("INSTANCE-READ-FAILED", "error", errorClass(err))
		return "unknown"
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256))
	id := strings.TrimSpace(string(body))
	if err != nil || resp.StatusCode != http.StatusOK || id == "" || strings.IndexFunc(id, notIDRune) >= 0 {
		log.Warn("INSTANCE-READ-FAILED", "status", resp.StatusCode)
		return "unknown"
	}
	return id
}

// runAll runs each part until ctx ends; the first part to fail stops the
// others, and runAll gives its error.
func runAll(ctx context.Context, parts ...func(context.Context) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errc := make(chan error, len(parts))
	var wg sync.WaitGroup
	for _, part := range parts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := part(ctx); err != nil {
				errc <- err
				cancel()
			}
		}()
	}
	wg.Wait()
	close(errc)
	return <-errc
}

// settings are C6's bridge settings.
type settings struct {
	PiAddress  string // BRIDGE_PI_ADDRESS: the Pi's tailnet address
	PiName     string // BRIDGE_PI_NAME: the name the Pi's certificate carries
	PiAudience string // BRIDGE_PI_AUDIENCE: the audience of the bridge's ID token

	OrgDomains   []string // BRIDGE_ORG_DOMAINS: the domains whose users may organize a call's event
	Testers      []string // BRIDGE_TESTERS: when set, the only organizers taken
	CalendarUser string   // BRIDGE_CALENDAR_USER: the bot's Workspace user

	AttendeeURL    string // BRIDGE_ATTENDEE_URL
	AttendeeAPIKey string // BRIDGE_ATTENDEE_API_KEY
	WebhookSecret  string // BRIDGE_WEBHOOK_SECRET
	BridgeKey      []byte // BRIDGE_KEY: the key of each bot's socket secret

	TLSCert    string // BRIDGE_TLS_CERT: the audio socket's certificate, PEM
	TLSKeyFile string // BRIDGE_TLS_KEY_FILE: the file of its key

	AudioPort   int // BRIDGE_AUDIO_PORT
	WebhookPort int // BRIDGE_WEBHOOK_PORT
	HealthPort  int // BRIDGE_HEALTH_PORT

	RunSpecFile string // BRIDGE_RUN_SPEC_FILE: the test pool's run spec; empty in the call pool

	// ProbeDNSAddress is BRIDGE_PROBE_DNS_ADDRESS, host:port of the DNS
	// server probe_network queries; optional (A4).
	ProbeDNSAddress string

	// MetadataURL is the base URL of Cloud Run's metadata server. Not a
	// setting: a test points it at its fake.
	MetadataURL string
}

const metadataURL = "http://metadata.google.internal"

// settingsRefusal names each setting the bridge refused and why: each item
// is KEY=unset or KEY=malformed, never the setting's value (C9).
type settingsRefusal []string

func (r settingsRefusal) Error() string { return strings.Join(r, " ") }

// refusedSettings gives the refused settings of err for the SETTINGS-REFUSED
// line.
func refusedSettings(err error) string {
	var r settingsRefusal
	if errors.As(err, &r) {
		return r.Error()
	}
	return "unknown"
}

// settingsFromEnv reads C6's bridge settings. Every one is required except
// BRIDGE_TESTERS (empty after the tester period), BRIDGE_RUN_SPEC_FILE (the
// test pool's alone) and BRIDGE_PROBE_DNS_ADDRESS (probe_network's). Its
// error is a settingsRefusal.
func settingsFromEnv(getenv func(string) string) (settings, error) {
	var refused settingsRefusal
	str := func(key string) string {
		v := strings.TrimSpace(getenv(key))
		if v == "" {
			refused = append(refused, key+"=unset")
		}
		return v
	}
	port := func(key string) int {
		v := str(key)
		if v == "" {
			return 0
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 65535 {
			refused = append(refused, key+"=malformed")
			return 0
		}
		return n
	}
	s := settings{
		PiAddress:       str("BRIDGE_PI_ADDRESS"),
		PiName:          str("BRIDGE_PI_NAME"),
		PiAudience:      str("BRIDGE_PI_AUDIENCE"),
		OrgDomains:      list(getenv("BRIDGE_ORG_DOMAINS")),
		Testers:         list(getenv("BRIDGE_TESTERS")),
		CalendarUser:    str("BRIDGE_CALENDAR_USER"),
		AttendeeURL:     str("BRIDGE_ATTENDEE_URL"),
		AttendeeAPIKey:  str("BRIDGE_ATTENDEE_API_KEY"),
		WebhookSecret:   str("BRIDGE_WEBHOOK_SECRET"),
		BridgeKey:       []byte(str("BRIDGE_KEY")),
		TLSCert:         getenv("BRIDGE_TLS_CERT"),
		TLSKeyFile:      str("BRIDGE_TLS_KEY_FILE"),
		AudioPort:       port("BRIDGE_AUDIO_PORT"),
		WebhookPort:     port("BRIDGE_WEBHOOK_PORT"),
		HealthPort:      port("BRIDGE_HEALTH_PORT"),
		RunSpecFile:     strings.TrimSpace(getenv("BRIDGE_RUN_SPEC_FILE")),
		ProbeDNSAddress: strings.TrimSpace(getenv("BRIDGE_PROBE_DNS_ADDRESS")),
		MetadataURL:     metadataURL,
	}
	if len(s.OrgDomains) == 0 {
		refused = append(refused, "BRIDGE_ORG_DOMAINS=unset")
	}
	if strings.TrimSpace(s.TLSCert) == "" {
		refused = append(refused, "BRIDGE_TLS_CERT=unset")
	}
	if len(refused) > 0 {
		return settings{}, refused
	}
	return s, nil
}

// list splits a comma-separated setting, dropping empty items.
func list(v string) []string {
	var out []string
	for _, item := range strings.Split(v, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// healthHandler answers the instance's startup probe.
func healthHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok\n")
	})
	return mux
}

// newServer is one of the bridge's HTTP servers: its requests end with ctx,
// the service's, so no request outlives the server's stop; and net/http's own
// reports go to a marked line (C9).
func newServer(ctx context.Context, name string, h http.Handler, log *slog.Logger) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
		ErrorLog:          serverErrorLog(log, name),
	}
}

// serverErrorLog is net/http's log of a server's own failures, a TLS
// handshake, a panic in a handler, an accept, as one marked line each that
// names the server and the failure's kind. It never copies net/http's text,
// which names the client's address and can hold what a request carried.
func serverErrorLog(log *slog.Logger, server string) *stdlog.Logger {
	return stdlog.New(serverErrorWriter{log: log, server: server}, "", 0)
}

type serverErrorWriter struct {
	log    *slog.Logger
	server string
}

func (w serverErrorWriter) Write(p []byte) (int, error) {
	kind := "other"
	switch {
	case bytes.Contains(p, []byte("TLS handshake error")):
		kind = "tls_handshake"
	case bytes.Contains(p, []byte("panic serving")):
		kind = "panic"
	case bytes.Contains(p, []byte("Accept error")):
		kind = "accept"
	}
	w.log.Warn("SERVER-ERROR server=" + w.server + " kind=" + kind)
	return len(p), nil
}

// serveHealth serves /healthz on ln until ctx ends.
func serveHealth(ctx context.Context, ln net.Listener, log *slog.Logger) error {
	srv := newServer(ctx, "health", healthHandler(), log)
	return serveUntil(ctx, srv, func() error { return srv.Serve(ln) }, log, "health")
}

// serveWebhooks serves Attendee's webhooks on ln until ctx ends.
func serveWebhooks(ctx context.Context, ln net.Listener, receiver http.Handler, log *slog.Logger) error {
	mux := http.NewServeMux()
	mux.Handle(webhookPath, receiver)
	srv := newServer(ctx, "webhook", mux, log)
	return serveUntil(ctx, srv, func() error { return srv.Serve(ln) }, log, "webhook")
}

// serveUntil runs serve, srv's listen loop, until ctx ends, then shuts srv
// down.
func serveUntil(ctx context.Context, srv *http.Server, serve func() error, log *slog.Logger, name string) error {
	errc := make(chan error, 1)
	go func() { errc <- serve() }()
	select {
	case err := <-errc:
		return &partError{name + " server", err}
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Warn("SERVER-SHUTDOWN-FAILED", "server", name, "error", errorClass(err))
	}
	return nil
}
