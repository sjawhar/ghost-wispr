package voice

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

// TestCloseWaitsForItsGoroutines fails if anything the Supervisor started
// still runs, and logs, after Close returns: a test's log after it ended is a
// data race, and shutdown must leave nothing behind.
func TestCloseWaitsForItsGoroutines(t *testing.T) {
	var closed atomic.Bool
	var late atomic.Int32
	var mu sync.Mutex
	c := newFakeConcierge()
	r := newRigWith(t, c, func(cfg *SupervisorConfig, _ *rig) {
		cfg.Logf = func(string, ...any) {
			mu.Lock()
			defer mu.Unlock()
			if closed.Load() {
				late.Add(1)
			}
		}
	})
	release := r.speaker.holdBegins(audio.InputChime)
	r.heard("Wispr,", false, false)
	r.waitSession()
	go func() {
		time.Sleep(50 * time.Millisecond)
		release()
	}()
	r.s.Close()
	closed.Store(true)
	time.Sleep(200 * time.Millisecond)
	if n := late.Load(); n > 0 {
		t.Fatalf("%d log lines came after Close returned", n)
	}
}
