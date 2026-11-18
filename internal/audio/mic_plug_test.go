package audio

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/gordonklaus/portaudio"
)

// fakePortAudioJabra stands in for PortAudio's ALSA host on the office Pi with
// the Jabra Speak2 75 as card 0, whose capture device takes only 32000 Hz
// (/proc/asound/card0/stream0: "Rates: 32000"), and the ALSA loopback as card
// 3. Like Pa_Initialize (PortAudio 19.6, pa_linux_alsa.c:1250-1256), it lists
// each card's devices as the raw "hw:N,M", or as "plughw:N,M", ALSA's plug
// layer, when PA_ALSA_PLUGHW is set. A raw device opens only at the rate the
// card takes; a plug device opens at any rate, which ALSA converts. It
// records each device opened, with its rate.
func fakePortAudioJabra(t *testing.T) *[]string {
	t.Helper()
	originalDevices, originalOpen := paInputDevices, paOpenInput
	t.Cleanup(func() { paInputDevices, paOpenInput = originalDevices, originalOpen })
	opened := new([]string)
	paInputDevices = func() ([]*portaudio.DeviceInfo, error) {
		prefix := "hw"
		if os.Getenv("PA_ALSA_PLUGHW") == "1" {
			prefix = "plughw"
		}
		return []*portaudio.DeviceInfo{
			{Name: "default", MaxInputChannels: 32},
			{Name: "Jabra Speak2 75: USB Audio (" + prefix + ":0,0)", MaxInputChannels: 1},
			{Name: "Loopback: PCM (" + prefix + ":3,0)", MaxInputChannels: 32},
		}, nil
	}
	paOpenInput = func(params portaudio.StreamParameters, _ *[]int16) (inputStream, error) {
		name, rate := params.Input.Device.Name, int(params.SampleRate)
		if strings.Contains(name, "(hw:0,0)") && rate != 32000 {
			return nil, errors.New("Invalid sample rate")
		}
		*opened = append(*opened, fmt.Sprintf("%s at %d", name, rate))
		return newHeldStream(), nil
	}
	return opened
}

// TestANamedMicrophoneOpensAt16000ThroughThePlugLayer fails if, with
// mic_device naming a card whose capture device takes only 32000 Hz, the
// microphone does not open at 16000 Hz, the rate the start tries first and the
// rate Deepgram, the recording, its recovery encode, the preroll and the
// reconnect buffer all assume, through ALSA's plug layer. On the office Pi,
// mic_device: J75 opened PortAudio's raw "(hw:0,0)", where 16000 failed and
// the microphone opened at 32000 (PiAcceptance.md §13).
func TestANamedMicrophoneOpensAt16000ThroughThePlugLayer(t *testing.T) {
	t.Setenv("PA_ALSA_PLUGHW", "") // restored after the test
	_ = os.Unsetenv("PA_ALSA_PLUGHW")
	officePiCards(t, true)
	opened := fakePortAudioJabra(t)

	if err := UsePlugCapture("J75"); err != nil {
		t.Fatalf("UsePlugCapture: %v", err)
	}
	if _, err := openInputStream("J75", 16000, 4000, new([]int16)); err != nil {
		t.Fatalf("mic_device J75 at 16000 Hz = %v, want it opened through the plug layer", err)
	}
	if want := "Jabra Speak2 75: USB Audio (plughw:0,0) at 16000"; len(*opened) != 1 || (*opened)[0] != want {
		t.Fatalf("opened %v, want %q only", *opened, want)
	}
}

// TestUsePlugCaptureLeavesAnUnsetMicDeviceAsBefore fails if UsePlugCapture
// changes PortAudio's device names while mic_device is unset: the default
// input, and its fallback, open as before the setting existed.
func TestUsePlugCaptureLeavesAnUnsetMicDeviceAsBefore(t *testing.T) {
	t.Setenv("PA_ALSA_PLUGHW", "")
	_ = os.Unsetenv("PA_ALSA_PLUGHW")
	if err := UsePlugCapture(""); err != nil {
		t.Fatalf("UsePlugCapture: %v", err)
	}
	if v, set := os.LookupEnv("PA_ALSA_PLUGHW"); set {
		t.Fatalf("UsePlugCapture with mic_device unset set PA_ALSA_PLUGHW=%q", v)
	}
}

// TestANamedMicrophoneNeverOpensARawDeviceOrAnotherCard fails if, when
// PortAudio lists the named card only as its raw "hw:N,M" device (PortAudio
// initialized without the plug layer), the microphone opens that device at
// whatever rate the card takes, rather than failing: every 16000 Hz assumption
// would then be wrong. It also fails if the open falls back to another card,
// such as the ALSA loopback, which the plug layer would open at any rate.
func TestANamedMicrophoneNeverOpensARawDeviceOrAnotherCard(t *testing.T) {
	t.Setenv("PA_ALSA_PLUGHW", "")
	_ = os.Unsetenv("PA_ALSA_PLUGHW")
	officePiCards(t, true)
	opened := fakePortAudioJabra(t)
	for _, rate := range []int{16000, 48000, 44100, 32000, 24000} {
		if _, err := openInputStream("J75", rate, rate/4, new([]int16)); err == nil {
			t.Fatalf("mic_device J75 opened %v at %d Hz with no plug device listed, want an error", *opened, rate)
		}
	}
	if len(*opened) > 0 {
		t.Fatalf("opened %v, want nothing", *opened)
	}

	// With the plug layer, a card that is not there opens nothing, though
	// the loopback's plug device would open at any rate.
	officePiCards(t, false)
	if err := UsePlugCapture("J75"); err != nil {
		t.Fatalf("UsePlugCapture: %v", err)
	}
	if _, err := openInputStream("J75", 16000, 4000, new([]int16)); err == nil {
		t.Fatalf("mic_device J75 with the Jabra unplugged opened %v, want an error", *opened)
	}
	if len(*opened) > 0 {
		t.Fatalf("opened %v, want nothing", *opened)
	}
}
