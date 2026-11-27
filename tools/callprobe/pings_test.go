package main

import (
	"fmt"
	"strings"
	"testing"
)

// H4 and c-link: `tailscale ping -c 1` lines on stdin give each target's p50
// and p99 of the reply times. A ping that timed out is no time, and neither is
// its "no reply" line (tailscale writes that one to stderr; a person may pipe
// it with 2>&1). A target that never replied is refused by name.
//
// The lines are tailscale's own, as cmd/tailscale/cli/ping.go prints them:
// "pong from <name> (<ip>[, <peerapi port>]) via <endpoint> in <latency>" and
// `ping "<ip>" timed out`.
func TestPingsGivesP50AndP99AndRefusesAnOfflineTarget(t *testing.T) {
	// 100 replies from the Pi: 1 ms to 99 ms and one of 1.5 s, in no order,
	// one in ten relayed through DERP; and two pings that timed out. Sorted,
	// the 50th reply is 50 ms and the 99th 99 ms. Were the two timeouts taken
	// as times of 0, the 50th of 102 would be 49 ms; as their 5 s timeout,
	// the 99th of 102 would be 1.5 s.
	var pi strings.Builder
	for i := range 100 {
		ms := (i*37)%100 + 1 // 37 and 100 share no factor: each of 1..100 once
		latency := fmt.Sprintf("%dms", ms)
		if ms == 100 {
			latency = "1.5s"
		}
		via := "192.0.2.10:41641"
		if i%10 == 0 {
			via = "DERP(nyc)"
		}
		peer := "100.64.0.7"
		if i == 5 {
			peer = "100.64.0.7, 41641" // a reply that names the peer's peerapi port
		}
		fmt.Fprintf(&pi, "pong from pi (%s) via %s in %s\n", peer, via, latency)
		if i == 40 || i == 80 {
			pi.WriteString("ping \"100.64.0.7\" timed out\nno reply\n")
		}
	}
	offline := strings.Repeat("ping \"100.64.0.9\" timed out\nno reply\n", 3)

	t.Run("a target that replies", func(t *testing.T) {
		out, errOut, code := callprobe(t, pi.String(), "pings")
		if code != 0 {
			t.Fatalf("pings exited %d: %s%s", code, out, errOut)
		}
		for key, want := range map[string]float64{
			"replies": 100, "timed_out": 2, "relayed": 10, "p50_ms": 50, "p99_ms": 99, "max_ms": 1500,
		} {
			if got := number(t, out, key); got != want {
				t.Errorf("%s=%v, want %v:\n%s", key, got, want, out)
			}
		}
	})

	t.Run("a target that never replied", func(t *testing.T) {
		out, errOut, code := callprobe(t, pi.String()+offline, "pings")
		if code == 0 {
			t.Errorf("pings exited 0 with a target that never replied:\n%s", out)
		}
		if !strings.Contains(errOut, "100.64.0.9") {
			t.Errorf("stderr does not name the target that never replied, 100.64.0.9:\n%s", errOut)
		}
		if got := number(t, out, "p99_ms"); got != 99 {
			t.Errorf("p99_ms=%v for the Pi beside an offline target, want 99:\n%s", got, out)
		}
	})

	t.Run("no ping line", func(t *testing.T) {
		out, errOut, code := callprobe(t, "no reply\npeer's node key has expired\n", "pings")
		if code == 0 {
			t.Errorf("pings exited 0 having read no ping line:\n%s", out)
		}
		if errOut == "" {
			t.Error("pings said nothing on stderr having read no ping line")
		}
	})
}
