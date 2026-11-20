//go:build !linux

package audio

import (
	"errors"
	"time"
)

// ALSACapture is the paused listener's capture, which needs ALSA: Linux only.
type ALSACapture struct{}

// ALSACapturePeriod is how much audio one Read waits for.
const ALSACapturePeriod = 100 * time.Millisecond

var errNoALSA = errors.New("ALSA capture needs Linux")

// OpenALSACapture fails: there is no ALSA here.
func OpenALSACapture(string, int) (*ALSACapture, error) { return nil, errNoALSA }

func (*ALSACapture) Read([]int16) (int, error) { return 0, errNoALSA }

func (*ALSACapture) Close() error { return nil }
