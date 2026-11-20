package audio

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCaptureDeviceFindsTheMicrophonesCard fails if the paused listener's ALSA
// PCM for the mic_device setting is not the card the recording microphone
// opens: the Jabra's capture PCM through ALSA's plug layer, which converts its
// 32 kHz to the listener's 16 kHz, or the loopback the acceptance driver
// injects speech on. A card that only plays is passed over.
func TestCaptureDeviceFindsTheMicrophonesCard(t *testing.T) {
	original := asoundRoot
	t.Cleanup(func() { asoundRoot = original })
	root := t.TempDir()
	asoundRoot = root
	cards := strings.Join([]string{
		" 0 [J75            ]: USB-Audio - Jabra Speak2 75",
		"                      GN Audio A/S Jabra Speak2 75 at usb-xhci-hcd.1-1, full speed",
		" 1 [vc4hdmi0       ]: vc4-hdmi - vc4-hdmi-0",
		"                      vc4-hdmi-0",
		" 2 [Loopback       ]: Loopback - Loopback",
		"                      Loopback 1",
		" 3 [Jplay          ]: USB-Audio - Jabra Evolve playback",
		"                      GN Audio A/S Jabra Evolve playback",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(root, "cards"), []byte(cards), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, dir := range [][]string{{"card0", "pcm0c"}, {"card0", "pcm0p"}, {"card1", "pcm0p"}, {"card2", "pcm0p"}, {"card2", "pcm1c"}, {"card3", "pcm0p"}} {
		if err := os.MkdirAll(filepath.Join(root, dir[0], dir[1]), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct{ name, want string }{
		{name: "Jabra", want: "plughw:CARD=J75,DEV=0"},
		{name: "Loopback", want: "plughw:CARD=Loopback,DEV=1"},
		{name: "", want: "default"},
		{name: "plughw:CARD=J75,DEV=0", want: "plughw:CARD=J75,DEV=0"},
		{name: "Evolve", want: "Evolve"}, // a card that only plays
	} {
		if got := CaptureDevice(tc.name); got != tc.want {
			t.Errorf("CaptureDevice(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}
