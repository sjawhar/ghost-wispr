package envoy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// staticKey is a signing key reader that always reads key.
func staticKey(key string) KeyReader {
	return func(context.Context) (string, error) { return key, nil }
}

// keySequence reads the keys in order, the last one from then on, and
// counts the reads.
type keySequence struct {
	mu    sync.Mutex
	keys  []string
	errs  []error
	reads int
}

func (k *keySequence) read(context.Context) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	i := min(k.reads, len(k.keys)-1)
	k.reads++
	var err error
	if i < len(k.errs) {
		err = k.errs[i]
	}
	return k.keys[i], err
}

func (k *keySequence) count() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.reads
}

// TestTheSigningKeyIsReadOnceAndKept fails if the client reads the key for
// every event, or signs with anything but the key it read.
func TestTheSigningKeyIsReadOnceAndKept(t *testing.T) {
	server, requests := newListenerTestServer(t, "signing-key")
	defer server.Close()
	keys := &keySequence{keys: []string{"signing-key\n"}}
	client := NewClient(server.URL, keys.read)
	client.retryDelays = nil

	for range 3 {
		if err := client.Send(context.Background(), Event{Type: eventSessionStarted, SessionID: "session-1"}); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}
	if got := len(requests()); got != 3 {
		t.Fatalf("listener accepted %d events, want 3", got)
	}
	if keys.count() != 1 {
		t.Fatalf("key reads = %d, want 1", keys.count())
	}
}

// TestA401ReadsTheKeyAgainOnce fails if a rotated key needs a restart: the
// listener refuses the old key with 401, so the client reads the key again
// and sends with the new one. A second 401 is not followed by a third read.
func TestA401ReadsTheKeyAgainOnce(t *testing.T) {
	server, requests := newListenerTestServer(t, "rotated-key")
	defer server.Close()
	keys := &keySequence{keys: []string{"old-key", "rotated-key"}}
	client := NewClient(server.URL, keys.read)
	client.retryDelays = nil

	if err := client.Send(context.Background(), Event{Type: eventSessionEnded, SessionID: "session-1"}); err != nil {
		t.Fatalf("Send after a rotation: %v", err)
	}
	if keys.count() != 2 {
		t.Fatalf("key reads after a 401 = %d, want 2", keys.count())
	}
	if got := len(requests()); got != 1 {
		t.Fatalf("listener accepted %d events, want 1", got)
	}
	if err := client.Send(context.Background(), Event{Type: eventSessionStarted, SessionID: "session-2"}); err != nil {
		t.Fatalf("Send with the kept rotated key: %v", err)
	}
	if keys.count() != 2 {
		t.Fatalf("key reads after a send with the rotated key = %d, want 2", keys.count())
	}

	// A key Envoy refuses even after the re-read: one more read, no loop.
	wrong, _ := newListenerTestServer(t, "envoys-key")
	defer wrong.Close()
	stale := &keySequence{keys: []string{"wrong-1", "wrong-2", "wrong-3"}}
	client = NewClient(wrong.URL, stale.read)
	client.retryDelays = nil
	err := client.Send(context.Background(), Event{Type: eventSessionEnded, SessionID: "session-1"})
	var status *webhookStatusError
	if !errors.As(err, &status) || status.status != http.StatusUnauthorized {
		t.Fatalf("Send with a key Envoy refuses: err = %v, want the 401", err)
	}
	if stale.count() != 2 {
		t.Fatalf("key reads for a refused key = %d, want 2", stale.count())
	}
}

// TestAFailedKeyReadSendsNothingAndIsReported fails if an event goes out
// unsigned or with no key, or if the failure is not reported: the unit shows
// it in /api/status. A later read that works clears it.
func TestAFailedKeyReadSendsNothingAndIsReported(t *testing.T) {
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		posts.Add(1)
	}))
	defer server.Close()
	readErr := errors.New("PERMISSION_DENIED")
	keys := &keySequence{keys: []string{"", "", "signing-key"}, errs: []error{readErr, nil}}
	var mu sync.Mutex
	var reported []error
	client := NewClient(server.URL, keys.read, WithKeyStatus(func(err error) {
		mu.Lock()
		defer mu.Unlock()
		reported = append(reported, err)
	}))
	client.retryDelays = nil

	for _, want := range []string{"PERMISSION_DENIED", "empty"} {
		err := client.Send(context.Background(), Event{Type: eventSessionStarted, SessionID: "session-1"})
		if err == nil || !errorsContains(err, want) {
			t.Fatalf("Send with an unreadable key: err = %v, want one naming %q", err, want)
		}
	}
	if posts.Load() != 0 {
		t.Fatalf("listener got %d posts with no key, want none", posts.Load())
	}
	if err := client.Send(context.Background(), Event{Type: eventSessionStarted, SessionID: "session-1"}); err != nil {
		t.Fatalf("Send once the key reads: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reported) != 3 || reported[0] == nil || reported[1] == nil || reported[2] != nil {
		t.Fatalf("reported key reads = %v, want two failures then a success", reported)
	}
}

func errorsContains(err error, text string) bool {
	return strings.Contains(strings.ToLower(err.Error()), strings.ToLower(text))
}
