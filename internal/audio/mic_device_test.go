package audio

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gordonklaus/portaudio"
)

// officePiCards writes the office Pi's sound cards during an injected-audio
// acceptance run (PiAcceptance.md §5 and §10.9) under a fresh asoundRoot: the
// HDMI outputs, the ALSA loopback (snd-aloop) as card 3, whose two devices
// both capture and play, and, with jabra set, the Jabra Speak2 75 as card 0.
func officePiCards(t *testing.T, jabra bool) {
	t.Helper()
	original := asoundRoot
	t.Cleanup(func() { asoundRoot = original })
	root := t.TempDir()
	asoundRoot = root
	lines := []string{
		" 1 [vc4hdmi0       ]: vc4-hdmi - vc4-hdmi-0",
		"                      vc4-hdmi-0",
		" 2 [vc4hdmi1       ]: vc4-hdmi - vc4-hdmi-1",
		"                      vc4-hdmi-1",
		" 3 [Loopback       ]: Loopback - Loopback",
		"                      Loopback 1",
	}
	pcms := [][]string{{"card1", "pcm0p"}, {"card2", "pcm0p"}, {"card3", "pcm0p"}, {"card3", "pcm0c"}, {"card3", "pcm1p"}, {"card3", "pcm1c"}}
	if jabra {
		lines = append([]string{
			" 0 [J75            ]: USB-Audio - Jabra Speak2 75",
			"                      GN Audio A/S Jabra Speak2 75 at usb-xhci-hcd.1-1, full speed",
		}, lines...)
		pcms = append(pcms, []string{"card0", "pcm0p"}, []string{"card0", "pcm0c"})
	}
	if err := os.WriteFile(filepath.Join(root, "cards"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, pcm := range pcms {
		if err := os.MkdirAll(filepath.Join(root, pcm[0], pcm[1]), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// TestCaptureDevicePresentLooksOnlyAtTheNamedCard fails if, with mic_device
// set, a capture PCM of another card counts as the microphone: on the office
// Pi with the ALSA loopback loaded and the Jabra unplugged, the Loopback card
// kept the start-up retry lighting red every 15 s (ReviewPR77 on #77, round
// 2, finding 5). The control: with mic_device unset, any capture PCM counts,
// as before the setting existed.
func TestCaptureDevicePresentLooksOnlyAtTheNamedCard(t *testing.T) {
	officePiCards(t, false)
	if !CaptureDevicePresent("") {
		t.Fatal("control: with mic_device unset, the Loopback card's capture PCM did not count")
	}
	if CaptureDevicePresent("Jabra") {
		t.Fatal("with mic_device Jabra and the Jabra unplugged, the Loopback card's capture PCM counts as the microphone")
	}
	if !CaptureDevicePresent("Loopback") {
		t.Fatal("with mic_device Loopback, the Loopback card's capture PCM did not count")
	}
	officePiCards(t, true)
	if !CaptureDevicePresent("Jabra") {
		t.Fatal("with mic_device Jabra and the Jabra plugged in, no capture device was found")
	}
}

// TestANamedMicrophoneOpensOnlyThatCardsInput fails if, with mic_device set,
// the microphone opens any input but the named card's: PortAudio's default
// input, or the first input that opens, can be the ALSA loopback, which would
// then be recorded with the light red (ReviewPR77 on #77, round 2, finding
// 5). It also fails if the named card's input is not the one opened, through
// PortAudio's device for its ALSA card and device numbers.
func TestANamedMicrophoneOpensOnlyThatCardsInput(t *testing.T) {
	originalDevices, originalOpen, originalDefault := paInputDevices, paOpenInput, paOpenDefaultInput
	t.Cleanup(func() {
		paInputDevices, paOpenInput, paOpenDefaultInput = originalDevices, originalOpen, originalDefault
	})
	// No case may reach a real input device: the default input is a stub too.
	defaultOpened := 0
	paOpenDefaultInput = func(int, int, *[]int16) (inputStream, error) {
		defaultOpened++
		return newHeldStream(), nil
	}
	// PortAudio's ALSA names with the plug layer (UsePlugCapture):
	// "<card name>: <device name> (plughw:<card>,<device>)".
	loopback := &portaudio.DeviceInfo{Name: "Loopback: PCM (plughw:3,0)", MaxInputChannels: 32}
	jabra := &portaudio.DeviceInfo{Name: "Jabra Speak2 75: USB Audio (plughw:0,0)", MaxInputChannels: 1}
	defaultInput := &portaudio.DeviceInfo{Name: "default", MaxInputChannels: 32}
	var opened []string
	paOpenInput = func(params portaudio.StreamParameters, _ *[]int16) (inputStream, error) {
		opened = append(opened, params.Input.Device.Name)
		return newHeldStream(), nil
	}

	for _, tc := range []struct {
		name    string
		jabra   bool
		devices []*portaudio.DeviceInfo
		want    string // the device opened; "" for none
	}{
		{name: "the Jabra is listed", jabra: true, devices: []*portaudio.DeviceInfo{defaultInput, loopback, jabra}, want: jabra.Name},
		{name: "the Jabra is unplugged", devices: []*portaudio.DeviceInfo{defaultInput, loopback}},
		{name: "the card is there, but PortAudio lists no input for it", jabra: true, devices: []*portaudio.DeviceInfo{defaultInput, loopback}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			officePiCards(t, tc.jabra)
			paInputDevices = func() ([]*portaudio.DeviceInfo, error) { return tc.devices, nil }
			opened = nil
			stream, err := openInputStream("Jabra", 16000, 4000, new([]int16))
			switch {
			case tc.want == "" && err == nil:
				t.Fatalf("mic_device Jabra opened %v with no Jabra input, want an error and nothing opened", opened)
			case tc.want != "" && (err != nil || stream == nil):
				t.Fatalf("mic_device Jabra = %v, want the Jabra's input opened", err)
			}
			if want := []string{tc.want}; tc.want == "" && len(opened) > 0 || tc.want != "" && (len(opened) != 1 || opened[0] != want[0]) {
				t.Fatalf("opened %v, want %q only", opened, tc.want)
			}
		})
	}

	if defaultOpened != 0 {
		t.Fatalf("mic_device Jabra opened the default input %d times", defaultOpened)
	}

	// Unset, the default input opens as before, and not the named-device path.
	paOpenInput = func(portaudio.StreamParameters, *[]int16) (inputStream, error) {
		return nil, errors.New("the named-device path ran for an unset mic_device")
	}
	if stream, err := openInputStream("", 16000, 4000, new([]int16)); err != nil || stream == nil || defaultOpened != 1 {
		t.Fatalf("unset mic_device = %v, %v, the default input opened %d times; want it opened once", stream, err, defaultOpened)
	}
}
