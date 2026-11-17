//go:build !linux

package audio

import (
	"fmt"

	"github.com/gordonklaus/portaudio"
)

// playbackUsesPortAudio says the speaker's playback streams are PortAudio's,
// so a playback holds PortAudio's output guard (outputs) while it is open.
var playbackUsesPortAudio = true

// openPlaybackStream opens a PortAudio output stream on systems without
// ALSA: the default output device, else the device named deviceName.
func openPlaybackStream(sampleRate, channels, framesPerBuffer int, buf []int16, deviceName string) (outputStream, error) {
	// Try default output device first.
	stream, err := portaudio.OpenDefaultStream(0, channels, float64(sampleRate), framesPerBuffer, buf)
	if err == nil {
		return stream, nil
	}

	// Fallback: enumerate devices and try ones with sufficient output channels.
	devices, devErr := portaudio.Devices()
	if devErr != nil {
		return nil, err
	}

	var firstErr error
	for _, dev := range devices {
		if dev == nil || dev.MaxOutputChannels < channels {
			continue
		}
		// If a specific device name was requested, only try matching devices.
		if deviceName != "" && dev.Name != deviceName {
			continue
		}

		params := portaudio.HighLatencyParameters(nil, dev)
		params.Output.Channels = channels
		params.SampleRate = float64(sampleRate)
		params.FramesPerBuffer = framesPerBuffer

		candidate, openErr := portaudio.OpenStream(params, buf)
		if openErr == nil {
			return candidate, nil
		}
		if firstErr == nil {
			firstErr = openErr
		}
	}

	if firstErr != nil {
		return nil, fmt.Errorf("default output failed: %w; fallback device open failed: %w", err, firstErr)
	}
	return nil, err
}
