package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
)

// serveHTTP relays signals to sig, then starts srv, and returns the channel
// its failure arrives on: a listen that fails (the address is taken) or a
// serve that ends with an error. Its Shutdown sends nothing. The handler is
// registered before the server starts: a SIGTERM that arrived while it
// started, with no handler yet, would end the process at once (exit 143),
// with no clean shutdown.
func serveHTTP(srv *http.Server, sig chan<- os.Signal, signals ...os.Signal) <-chan error {
	signal.Notify(sig, signals...)
	failed := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			failed <- fmt.Errorf("web server on %s: %w", srv.Addr, err)
		}
	}()
	return failed
}

// awaitStop waits until the recorder must stop: a signal (nil), or the web
// server's failure (its error). A recorder with no web UI stops, so systemd
// starts it again, rather than recording with nothing to control it.
func awaitStop(sig <-chan os.Signal, served <-chan error) error {
	select {
	case <-sig:
		return nil
	case err := <-served:
		return err
	}
}
