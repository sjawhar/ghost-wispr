package audio

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
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

// TestPlaybackDeviceFindsTheConfiguredCard fails if the speaker_device
// setting does not find the Jabra's ALSA playback device, or takes a card
// that matches but cannot play, or does not pass a name ALSA already reads as
// a PCM through. The cards are the office Pi's during its acceptance run
// (PiAcceptance.md §5 and §10.9): the Jabra Speak2 75 as card 0, id J75,
// whose playback PCM pcmC0D0p played the speech; the HDMI outputs; the
// injection loopback; plus a capture-only card whose name also says Jabra.
func TestPlaybackDeviceFindsTheConfiguredCard(t *testing.T) {
	original := asoundRoot
	t.Cleanup(func() { asoundRoot = original })
	root := t.TempDir()
	asoundRoot = root
	cards := strings.Join([]string{
		" 0 [J75            ]: USB-Audio - Jabra Speak2 75",
		"                      GN Audio A/S Jabra Speak2 75 at usb-xhci-hcd.1-1, full speed",
		" 1 [vc4hdmi0       ]: vc4-hdmi - vc4-hdmi-0",
		"                      vc4-hdmi-0",
		" 2 [vc4hdmi1       ]: vc4-hdmi - vc4-hdmi-1",
		"                      vc4-hdmi-1",
		" 3 [Loopback       ]: Loopback - Loopback",
		"                      Loopback 1",
		" 4 [Link           ]: USB-Audio - Jabra Link 390 capture",
		"                      GN Audio A/S Jabra Link 390 capture at usb-xhci-hcd.0-2, full speed",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(root, "cards"), []byte(cards), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, dir := range [][]string{{"card0", "pcm0c"}, {"card0", "pcm0p"}, {"card1", "pcm0p"}, {"card2", "pcm0p"}, {"card3", "pcm0p"}, {"card3", "pcm1c"}, {"card4", "pcm0c"}} {
		if err := os.MkdirAll(filepath.Join(root, dir[0], dir[1]), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	for _, tc := range []struct{ name, want string }{
		{name: "Jabra", want: "plughw:CARD=J75,DEV=0"},
		{name: "speak2", want: "plughw:CARD=J75,DEV=0"},
		{name: "Loopback", want: "plughw:CARD=Loopback,DEV=0"},
		{name: "", want: "default"},
		{name: "plughw:CARD=J75,DEV=0", want: "plughw:CARD=J75,DEV=0"},
		// Names no card with a playback device matches go to ALSA as PCM
		// names: its own, one asound.conf defines, or none (the open fails).
		{name: "default", want: "default"},
		{name: "null", want: "null"},
		{name: "pulse", want: "pulse"},
		{name: "Link 390", want: "Link 390"}, // matches a card that cannot play
	} {
		if got, _ := playbackDevice(tc.name); got != tc.want {
			t.Errorf("playbackDevice(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
	if _, cards := playbackDevice("Polycom"); !slices.Equal(cards, []string{"J75", "vc4hdmi0", "vc4hdmi1", "Loopback", "Link"}) {
		t.Errorf("cards looked through for Polycom = %v, want every card's id", cards)
	}
	asoundRoot = filepath.Join(root, "missing")
	if got, cards := playbackDevice("Jabra"); got != "Jabra" || cards != nil {
		t.Errorf("with no /proc/asound/cards, playbackDevice(Jabra) = %q, %v; want the name as it is", got, cards)
	}
}
