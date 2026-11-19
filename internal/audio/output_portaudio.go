package audio

import (
	"errors"
	"fmt"
	"strings"

	"github.com/gordonklaus/portaudio"
)

// portAudioOutputDevice is the PortAudio output device the speaker opens off
// Linux: with name, the speaker_device setting, the first device that can
// play whose name contains it, without case, and never the default instead;
// with none, the default output.
func portAudioOutputDevice(devices []*portaudio.DeviceInfo, defaultOutput *portaudio.DeviceInfo, name string) (*portaudio.DeviceInfo, error) {
	if name == "" {
		if defaultOutput == nil {
			return nil, errors.New("no default output device")
		}
		return defaultOutput, nil
	}
	want := strings.ToLower(name)
	var outputs []string
	for _, d := range devices {
		if d == nil || d.MaxOutputChannels < 1 {
			continue
		}
		if strings.Contains(strings.ToLower(d.Name), want) {
			return d, nil
		}
		outputs = append(outputs, d.Name)
	}
	return nil, fmt.Errorf("speaker_device %q matches no output device (devices: %s)", name, strings.Join(outputs, ", "))
}
