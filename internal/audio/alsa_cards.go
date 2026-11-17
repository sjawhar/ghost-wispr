package audio

import (
	"os"
	"path/filepath"
)

// asoundRoot is where the kernel lists ALSA's sound cards; tests point it at
// a directory of their own.
var asoundRoot = "/proc/asound"

// CaptureDevicePresent reports whether a capture device exists, without
// opening one: ALSA lists each capture PCM of each card as
// /proc/asound/cardN/pcmMc. The microphone opens PortAudio's default input,
// which on ALSA is one of them. On a system with no /proc/asound, which is not
// ALSA, it reports true: it cannot tell.
func CaptureDevicePresent() bool {
	if _, err := os.Stat(asoundRoot); err != nil {
		return true
	}
	captures, _ := filepath.Glob(filepath.Join(asoundRoot, "card[0-9]*", "pcm[0-9]*c"))
	return len(captures) > 0
}
