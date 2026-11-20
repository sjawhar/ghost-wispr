package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestAWebServerThatCannotListenStopsTheRecorder fails if the web server,
// whose address another process holds, leaves the recorder running with no
// web UI: the recorder must stop with the listen error, so systemd restarts
// it.
func TestAWebServerThatCannotListenStopsTheRecorder(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = taken.Close() })
	sig := make(chan os.Signal, 1)
	t.Cleanup(func() { signal.Stop(sig) })
	served := serveHTTP(&http.Server{Addr: taken.Addr().String(), Handler: http.NotFoundHandler()}, sig, syscall.SIGUSR2)
	stopped := make(chan error, 1)
	go func() { stopped <- awaitStop(sig, served) }()
	select {
	case err := <-stopped:
		if err == nil || !strings.Contains(err.Error(), "address already in use") {
			t.Fatalf("the recorder stopped with %v, want the listen error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a web server that cannot listen left the recorder running")
	}
}

// TestTheSignalHandlerIsReadyBeforeTheServerServes fails if a stop signal
// that arrives as soon as the web server is started (systemd's SIGTERM can)
// is not caught by the recorder: with the handler registered after the
// server starts, a SIGTERM then ended the process at once (exit 143), with
// no clean shutdown. The test sends SIGUSR1, which a Go process with no
// handler for it ignores, so a missed signal leaves awaitStop waiting rather
// than ending the test binary.
func TestTheSignalHandlerIsReadyBeforeTheServerServes(t *testing.T) {
	sig := make(chan os.Signal, 1)
	srv := &http.Server{Addr: "127.0.0.1:0", Handler: http.NotFoundHandler()}
	served := serveHTTP(srv, sig, syscall.SIGUSR1)
	t.Cleanup(func() {
		signal.Stop(sig)
		_ = srv.Close()
	})
	if err := syscall.Kill(os.Getpid(), syscall.SIGUSR1); err != nil {
		t.Fatal(err)
	}
	if err := awaitStopWithin(t, sig, served); err != nil {
		t.Fatalf("a signal at the server's start stopped the recorder with %v, want no error", err)
	}
}

// TestASignalStopsTheRecorderWithNoError fails if a web server that serves
// does not answer, if a stop signal does not stop the recorder with no error,
// or if the web server's shutdown is taken for a failure.
func TestASignalStopsTheRecorderWithNoError(t *testing.T) {
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := free.Addr().String()
	_ = free.Close()
	srv := &http.Server{Addr: addr, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })}
	sig := make(chan os.Signal, 1)
	t.Cleanup(func() { signal.Stop(sig) })
	served := serveHTTP(srv, sig, syscall.SIGUSR2)
	var body string
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if resp, err := http.Get("http://" + addr + "/"); err == nil {
			data, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			body = string(data)
			break
		}
	}
	if body != "ok" {
		t.Fatalf("the web server answered %q, want ok", body)
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGUSR2); err != nil {
		t.Fatal(err)
	}
	if err := awaitStopWithin(t, sig, served); err != nil {
		t.Fatalf("a stop signal stopped the recorder with %v, want no error", err)
	}
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-served:
		t.Fatalf("the web server's shutdown was reported as %v, want nothing", err)
	case <-time.After(100 * time.Millisecond):
	}
}

// awaitStopWithin is awaitStop, failing the test when it does not return
// within 5 s.
func awaitStopWithin(t *testing.T, sig <-chan os.Signal, served <-chan error) error {
	t.Helper()
	stopped := make(chan error, 1)
	go func() { stopped <- awaitStop(sig, served) }()
	select {
	case err := <-stopped:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("the recorder did not stop")
		return nil
	}
}
