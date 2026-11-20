package transcribe

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/logtest"
)

// blockingDestination is a live-transcription socket whose writes wait for
// release, recording what it got in order.
type blockingDestination struct {
	mu      sync.Mutex
	got     []string
	release chan struct{}
}

func (d *blockingDestination) Write(p []byte) (int, error) {
	<-d.release
	d.mu.Lock()
	defer d.mu.Unlock()
	d.got = append(d.got, string(p))
	return len(p), nil
}

func (d *blockingDestination) Finalize() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.got = append(d.got, "finalize")
	return nil
}

func (d *blockingDestination) calls() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.got)
}

// TestAsyncSenderDropsWhenFullWithoutBlocking fails if a write to a full
// send queue blocks the microphone loop, or if the chunks it kept are not
// sent in order once the socket takes them again.
func TestAsyncSenderDropsWhenFullWithoutBlocking(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dst := &blockingDestination{release: make(chan struct{})}
	logged := logtest.Capture(t)
	sender := NewAsyncSender(ctx, dst, 2, nil)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for _, chunk := range []string{"a", "b", "c", "d", "e"} {
			if n, err := sender.Write([]byte(chunk)); n != 1 || err != nil {
				t.Errorf("Write(%q) = %d, %v, want 1, nil", chunk, n, err)
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("writes to a full send queue blocked")
	}
	close(dst.release)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && len(dst.calls()) < 2 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	// A slot is free again only once its chunk is sent, so "a" and "b" took
	// the two slots and "c", "d" and "e" were dropped.
	if got := dst.calls(); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("socket got %q, want the two chunks the queue held, in order", got)
	}
	if !slices.ContainsFunc(logged.At(slog.LevelWarn), func(record logtest.Record) bool { return strings.Contains(record.Message, "dropping audio") }) {
		t.Fatalf("logs = %+v, want a warning saying live transcription drops audio", logged.Records())
	}
}

// TestAsyncSenderFinalizesAfterQueuedAudio fails if Finalize overtakes audio
// queued before it, so Deepgram finalizes before it has the pause's last
// words.
func TestAsyncSenderFinalizesAfterQueuedAudio(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dst := &blockingDestination{release: make(chan struct{})}
	sender := NewAsyncSender(ctx, dst, 4, nil)
	for _, chunk := range []string{"a", "b", "c"} {
		if _, err := sender.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	finalized := make(chan error, 1)
	go func() { finalized <- sender.Finalize() }()
	time.Sleep(20 * time.Millisecond)
	close(dst.release)
	select {
	case err := <-finalized:
		if err != nil {
			t.Fatalf("Finalize: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Finalize did not return")
	}
	if got, want := dst.calls(), []string{"a", "b", "c", "finalize"}; !slices.Equal(got, want) {
		t.Fatalf("socket calls = %q, want %q", got, want)
	}
}
