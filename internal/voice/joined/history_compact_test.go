package joined

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/envoy"
)

// The tests here hold the roster's memory of commands to a compact record
// with a hard bound, and its answer to a command that comes a moment before
// the unit's Envoy session is ready.

// heapAfterGC is the live heap once a collection has run, with r kept alive.
func heapAfterGC(r *Roster) uint64 {
	runtime.GC()
	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	runtime.KeepAlive(r)
	return stats.HeapAlloc
}

func mib(b uint64) float64 { return float64(b) / (1 << 20) }

// TestTheCommandHistoryStaysCompact fails if one command a second over
// Envoy's 72 h window (259,200 commands from 256 agents) holds more than
// 12 MiB, if the history at its bound (every one of the 256 at its cap) holds
// more than 24 MiB, if a command past the bound is refused rather than taken
// in place of its own session's oldest, if the history then grows past its
// bound or forgets a command it should keep, or if the memory stays held once
// every command has expired.
func TestTheCommandHistoryStaysCompact(t *testing.T) {
	r := newRig(t, func(cfg *Config) { cfg.Logf = func(string, ...any) {} })
	ctx := context.Background()
	start := r.clock.Now()
	base := heapAfterGC(r.roster)
	deliver := func(i int, at time.Time) error {
		r.clock.set(at)
		m := &envoy.Message{
			EventID:       fmt.Sprintf("00000000-0000-4000-8000-%012d", i),
			SourceSession: fmt.Sprintf("agent-%03d", i%256),
			Text:          "ghost-wispr leave",
			IssuedAt:      at,
		}
		_, err := r.roster.Deliver(ctx, m)
		return err
	}
	const perSecond = 72 * 60 * 60
	for i := range perSecond {
		if err := deliver(i, start.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatalf("command %d: %v", i, err)
		}
	}
	atRate := heapAfterGC(r.roster) - base
	t.Logf("one command a second for 72 h: %d commands hold %.2f MiB (%.1f bytes a command)", perSecond, mib(atRate), float64(atRate)/perSecond)
	if atRate > 12<<20 {
		t.Fatalf("the history of %d commands holds %.1f MiB, want at most 12 MiB", perSecond, mib(atRate))
	}
	// The rest of the bound, in the per-second run's last second, so every
	// command is still within Envoy's window: each of the 256 agents reaches
	// its cap.
	last := start.Add((perSecond - 1) * time.Second)
	for i := perSecond; i < maxCommands; i++ {
		if err := deliver(i, last); err != nil {
			t.Fatalf("command %d of the bound's %d: %v", i, maxCommands, err)
		}
	}
	atBound := heapAfterGC(r.roster) - base
	t.Logf("at the bound: %d commands hold %.2f MiB (%.1f bytes a command)", maxCommands, mib(atBound), float64(atBound)/maxCommands)
	if atBound > 24<<20 {
		t.Fatalf("the history at its bound of %d commands holds %.1f MiB, want at most 24 MiB", maxCommands, mib(atBound))
	}
	if err := deliver(maxCommands, last); err != nil {
		t.Fatalf("a command past the bound returned %v, want it taken in place of its session's oldest", err)
	}
	key := func(i int) commandKey {
		return commandKeyOf(fmt.Sprintf("agent-%03d", i%256), fmt.Sprintf("00000000-0000-4000-8000-%012d", i))
	}
	r.roster.mu.Lock()
	size, newest, second, first := r.roster.commands.size(), r.roster.commands.has("agent-000", key(maxCommands)),
		r.roster.commands.has("agent-000", key(256)), r.roster.commands.has("agent-000", key(0))
	r.roster.mu.Unlock()
	switch {
	case size != maxCommands:
		t.Fatalf("past its bound the history holds %d commands, want %d", size, maxCommands)
	case !newest || !second:
		t.Fatalf("past its bound the history forgot a command it should keep: the newest kept %v, agent-000's second kept %v", newest, second)
	case first:
		t.Fatal("past its bound the history kept agent-000's oldest command, in place of which it took the newest")
	}
	r.clock.set(last.Add(replayWindow))
	r.roster.Sweep(ctx)
	var after uint64
	if heap := heapAfterGC(r.roster); heap > base {
		after = heap - base
	}
	t.Logf("once every command expired: %.2f MiB", mib(after))
	if after > 1<<20 {
		t.Fatalf("once every command expired the history still holds %.1f MiB", mib(after))
	}
	if err := deliver(maxCommands+1, r.clock.Now()); err != nil {
		t.Fatalf("control: a command once every one expired was refused: %v", err)
	}
}

// TestANotReadyCommandWaitsForTheEnvoySession fails if a join that comes a
// moment before the roster has the unit's Envoy session is refused at once,
// rather than taken once the session is given; and if one with no session
// coming is refused before the roster's wait, which spaces Envoy's
// redeliveries whatever its own backoff.
func TestANotReadyCommandWaitsForTheEnvoySession(t *testing.T) {
	r := newRig(t, nil)
	r.opening.Close(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := r.roster.Deliver(context.Background(), &envoy.Message{EventID: "e-early", SourceSession: "agent-1", Text: "ghost-wispr join ABC-12", IssuedAt: r.clock.Now()})
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	r.open(r.agents)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a join that came a moment before the Envoy session was given returned %v, want it taken", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the join was still waiting 5 s after the Envoy session was given")
	}
	if got := r.roster.Joined(); len(got) != 1 {
		t.Fatalf("roster %+v, want the early join taken", got)
	}

	idle := newRig(t, nil)
	idle.opening.Close(context.Background())
	began := time.Now()
	_, err := idle.roster.Deliver(context.Background(), &envoy.Message{EventID: "e-none", SourceSession: "agent-2", Text: "ghost-wispr join XYZ-9", IssuedAt: idle.clock.Now()})
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("a join with no Envoy session coming returned %v, want ErrNotReady", err)
	}
	if waited := time.Since(began); waited < time.Second {
		t.Fatalf("a join with no Envoy session was refused after %s, want the roster's wait of at least 1 s first", waited)
	}
}
