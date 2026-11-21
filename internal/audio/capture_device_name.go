package audio

import (
	"fmt"
	"strings"
)

// CaptureDevice returns the ALSA PCM the paused listener captures from for
// name, the mic_device setting: the card the recording microphone opens.
//   - "" is ALSA's "default", and a name with a colon, as plughw:CARD=J75,
//     is a PCM, used as it is;
//   - any other name names a sound card (findCard), whose first capture PCM
//     gives plughw:CARD=<id>,DEV=<n>, which converts the card's rate to the
//     one the listener asks for, as the recording microphone's plug device
//     does (UsePlugCapture);
//   - a name no card with a capture PCM matches is used as it is.
func CaptureDevice(name string) string {
	if name == "" {
		return "default"
	}
	if strings.Contains(name, ":") {
		return name
	}
	_, id, dev, _, found := findCard(name, "c")
	if !found {
		return name
	}
	return fmt.Sprintf("plughw:CARD=%s,DEV=%s", id, dev)
}
