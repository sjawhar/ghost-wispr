package main

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/ghost-wispr/tools/callprobe/analyze"
)

// The lines of `tailscale ping`, as cmd/tailscale/cli/ping.go prints them on
// stdout: a reply, "pong from <name> (<ip>[, <peerapi port>]) via <endpoint,
// DERP(<region>) or peer-relay(<relay>)> in <latency>", and a ping with no
// reply, `ping "<ip>" timed out`. Its "no reply" goes to stderr, and names no
// target: it is no time, and counts for nothing.
var (
	pongLine    = regexp.MustCompile(`^pong from (\S+) \(([^,)]+)(?:, \d+)?\) via (\S+) in (\S+)$`)
	timeoutLine = regexp.MustCompile(`^ping "([^"]+)" timed out$`)
)

// pingTarget is what the lines say of one target.
type pingTarget struct {
	ip, name string
	replies  []time.Duration
	timedOut int
	relayed  int // replies through a relay (DERP or a peer relay), not a direct path
}

func runPings(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		_, _ = fmt.Fprintf(stderr, "callprobe pings: no arguments; give tailscale ping lines on stdin\n")
		return 2
	}
	var targets []*pingTarget
	target := func(ip string) *pingTarget {
		for _, t := range targets {
			if t.ip == ip {
				return t
			}
		}
		t := &pingTarget{ip: ip}
		targets = append(targets, t)
		return t
	}
	read, unread := 0, 0
	scanner := bufio.NewScanner(stdin)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		read++
		if m := pongLine.FindStringSubmatch(line); m != nil {
			latency, err := time.ParseDuration(m[4])
			if err != nil {
				_, _ = fmt.Fprintf(stderr, "callprobe pings: %q: a latency that is no duration\n", line)
				return 1
			}
			t := target(m[2])
			t.name = m[1]
			t.replies = append(t.replies, latency)
			if strings.HasPrefix(m[3], "DERP(") || strings.HasPrefix(m[3], "peer-relay(") {
				t.relayed++
			}
			continue
		}
		if m := timeoutLine.FindStringSubmatch(line); m != nil {
			target(m[1]).timedOut++
			continue
		}
		unread++ // "no reply", and tailscale's other messages on stderr
	}
	if err := scanner.Err(); err != nil {
		_, _ = fmt.Fprintf(stderr, "callprobe pings: reading stdin: %v\n", err)
		return 1
	}
	if len(targets) == 0 {
		_, _ = fmt.Fprintf(stderr, "callprobe pings: no reply and no timed-out ping in %d lines: give the stdout of tailscale ping\n", read)
		return 1
	}
	silent := 0
	for _, t := range targets {
		pings := len(t.replies) + t.timedOut
		if len(t.replies) == 0 {
			_, _ = fmt.Fprintf(stdout, "target=%s pings=%d replies=0 timed_out=%d (never replied)\n", t.ip, pings, t.timedOut)
			_, _ = fmt.Fprintf(stderr, "callprobe pings: %s never replied: %d pings timed out\n", t.ip, t.timedOut)
			silent++
			continue
		}
		slices.Sort(t.replies)
		_, _ = fmt.Fprintf(stdout, "target=%s name=%s pings=%d replies=%d timed_out=%d relayed=%d p50_ms=%s p99_ms=%s max_ms=%s\n",
			t.ip, t.name, pings, len(t.replies), t.timedOut, t.relayed,
			wholeMS(analyze.Percentile(t.replies, 50)), wholeMS(analyze.Percentile(t.replies, 99)), wholeMS(t.replies[len(t.replies)-1]))
	}
	_, _ = fmt.Fprintf(stdout, "lines=%d other_lines=%d\n", read, unread)
	if silent > 0 {
		return 1
	}
	return 0
}

// wholeMS is a latency tailscale rounded to the millisecond, in milliseconds.
func wholeMS(d time.Duration) string { return fmt.Sprint(d.Milliseconds()) }
