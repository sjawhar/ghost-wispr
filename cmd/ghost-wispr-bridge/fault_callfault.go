//go:build callfault

package main

import (
	"context"
	"crypto/x509"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/call"
)

// bridgeBuild is the build this bridge names in its hello: the test pool's,
// which joins the run spec's Meet link and runs the run spec's faults.
const bridgeBuild = call.BuildCallfault

// publicDNS is where probe_network sends its 100 UDP packets (plan C8), so
// the call subnet's flow log shows whether it records UDP.
const publicDNS = "8.8.8.8:53"

// faultEnv is where the faults reach outside the bridge's process: the
// public DNS address of probe_network, the roots that check the Pi's
// certificate (nil: the system's), and the process's exit.
type faultEnv struct {
	publicDNS string
	rootCAs   *x509.CertPool
	exit      func(code int)
}

// applyFaults runs before runBridge. It reads the run spec once and refuses
// a bad or expired one (stop true: no bot and no link); else it sets the
// bridge's calendar to the run spec's one event, its link to a fake Pi for
// fake_pi, its faults, and starts probe_network and exit_bridge.
//
//nolint:gocritic // hugeParam: the contract passes settings by value
func applyFaults(ctx context.Context, s settings, deps *bridgeDeps) (stop bool, err error) {
	return applyRunSpec(ctx, &s, deps, faultEnv{publicDNS: publicDNS, exit: os.Exit})
}

// applyRunSpec is applyFaults with its outside world in env. A refused run
// spec logs RUN-SPEC-REFUSED and changes nothing; it is no error, so main
// keeps the bridge up, idle, and Cloud Run does not restart it in a loop.
func applyRunSpec(ctx context.Context, s *settings, deps *bridgeDeps, env faultEnv) (bool, error) {
	now := deps.Clock.Now()
	spec, refusal := readRunSpec(s.RunSpecFile, now)
	if refusal != nil {
		deps.Log.Error("RUN-SPEC-REFUSED reason=" + refusal.reason)
		return true, nil
	}

	deps.Calendar = newRunSpecCalendar(&spec, now)
	deps.Faults = faultSet{
		ChatCheckAlwaysPasses: spec.has(faultChatCheckAlwaysPasses),
		NoticeJoinOnly:        spec.has(faultNoticeJoinOnly),
		LongLines:             spec.has(faultLongLines),
		LeaveAt:               spec.EndTime,
	}
	if spec.has(faultFakePi) {
		deps.Link = newFakePi(ctx, deps.Clock, deps.Log)
	}
	deps.Log.Info("RUN-SPEC faults=" + strings.Join(spec.Fault, ",") + " end_time=" + spec.EndTime.Format(time.RFC3339))

	if spec.has(faultProbeNetwork) {
		targets := probeTargetsFor(s, env)
		go probeNetwork(ctx, &targets, deps.Log)
	}
	if spec.has(faultExitBridge) {
		after := spec.FaultAfterS
		fire := deps.Clock.After(time.Duration(after) * time.Second)
		go func() {
			select {
			case <-fire:
				deps.Log.Error("FAULT exit_bridge after_s=" + strconv.Itoa(after))
				env.exit(1)
			case <-ctx.Done():
			}
		}()
	}
	return false, nil
}
