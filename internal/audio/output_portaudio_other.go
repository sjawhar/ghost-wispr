//go:build !linux

package audio

import (
	"fmt"
	"time"

	"github.com/gordonklaus/portaudio"
)

// playbackUsesPortAudio says the speaker's stream is PortAudio's, so the
// Output holds PortAudio's output guard (outputs) while its stream is open.
var playbackUsesPortAudio = true

// openSpeakerStream opens a PortAudio output stream, on systems without ALSA,
// on the device speaker_device names (portAudioOutputDevice), at its default
// rate, with frames of outputFrame. PortAudio must be initialized.
func openSpeakerStream(deviceName string) (PlaybackStream, int, error) {
	devices, err := portaudio.Devices()
	if err != nil {
		return nil, 0, fmt.Errorf("list output devices: %w", err)
	}
	defaultOutput, _ := portaudio.DefaultOutputDevice()
	dev, err := portAudioOutputDevice(devices, defaultOutput, deviceName)
	if err != nil {
		return nil, 0, err
	}
	rate := int(dev.DefaultSampleRate)
	channels := min(2, dev.MaxOutputChannels)
	frames := rate * int(outputFrame/time.Millisecond) / 1000
	buf := make([]int16, frames*channels)
	params := portaudio.LowLatencyParameters(nil, dev)
	params.Output.Channels = channels
	params.SampleRate = float64(rate)
	params.FramesPerBuffer = frames
	stream, err := portaudio.OpenStream(params, buf)
	if err != nil {
		return nil, 0, fmt.Errorf("open output stream on %s: %w", dev.Name, err)
	}
	if err := stream.Start(); err != nil {
		_ = stream.Close()
		return nil, 0, fmt.Errorf("start output stream on %s: %w", dev.Name, err)
	}
	return &portAudioStream{stream: stream, buf: buf, channels: channels}, rate, nil
}

// portAudioStream is the speaker's PlaybackStream on a PortAudio output
// stream that plays buf on each write.
type portAudioStream struct {
	stream   *portaudio.Stream
	buf      []int16
	channels int
}

// Write plays frame, one outputFrame, on every channel.
func (s *portAudioStream) Write(frame []int16) error {
	clear(s.buf)
	for i, v := range frame[:min(len(frame), len(s.buf)/s.channels)] {
		for c := range s.channels {
			s.buf[i*s.channels+c] = v
		}
	}
	return s.stream.Write()
}

// Drop drops nothing: a Cancel off Linux waits for PortAudio's own buffer.
func (s *portAudioStream) Drop() (int, error) { return 0, nil }

// Close aborts the stream, dropping what it holds, and closes it.
func (s *portAudioStream) Close() error {
	_ = s.stream.Abort()
	return s.stream.Close()
}
