//go:build !kws || !linux

package local

import "errors"

// Built reports whether this build has the paused listener's spotter. A build
// without the kws tag has none: the unit is the two-state machine, and every
// pause closes the microphone.
const Built = false

// ErrNotBuilt is NewDetector's error in a build with no spotter.
var ErrNotBuilt = errors.New("this build has no paused listener (built without -tags kws)")

// NewDetector fails: this build has no spotter.
func NewDetector() (*Detector, error) { return nil, ErrNotBuilt }

// NewDetectorWith fails: this build has no spotter.
func NewDetectorWith(Options) (*Detector, error) { return nil, ErrNotBuilt }
