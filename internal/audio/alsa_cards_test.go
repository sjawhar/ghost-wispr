package audio

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCaptureDevicePresentReadsALSAsCardList fails if a capture device is
// found where /proc/asound lists only playback PCMs, as on a Pi 5 with no
// Jabra (its HDMI cards play only), or is missed where it lists a capture PCM,
// as the Jabra's card does, or if a system with no /proc/asound holds the
// microphone back.
func TestCaptureDevicePresentReadsALSAsCardList(t *testing.T) {
	original := asoundRoot
	t.Cleanup(func() { asoundRoot = original })
	root := t.TempDir()
	asoundRoot = root
	for _, dir := range [][]string{{"card0", "pcm0p"}, {"card1", "pcm0p"}, {"card1", "pcm1p"}} {
		if err := os.MkdirAll(filepath.Join(root, dir[0], dir[1]), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if CaptureDevicePresent() {
		t.Fatal("found a capture device where ALSA lists only playback PCMs")
	}
	if err := os.MkdirAll(filepath.Join(root, "card2", "pcm0c"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !CaptureDevicePresent() {
		t.Fatal("missed the capture PCM ALSA lists as card2/pcm0c")
	}
	asoundRoot = filepath.Join(root, "missing")
	if !CaptureDevicePresent() {
		t.Fatal("a system with no /proc/asound reported no capture device, which holds the microphone back")
	}
}
