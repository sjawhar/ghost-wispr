//go:build !callfault

package main

import (
	"context"

	"github.com/sjawhar/ghost-wispr/internal/call"
)

// bridgeBuild is the build this bridge names in its hello. The release build
// joins only from the calendar (R14): it has no run spec and no fault.
const bridgeBuild = call.BuildRelease

// applyFaults runs before runBridge. In the release build it reads no run
// spec and changes nothing; the callfault build's applyFaults
// (fault_callfault.go) sets the test pool's faults.
func applyFaults(context.Context, settings, *bridgeDeps) (stop bool, err error) {
	return false, nil
}
