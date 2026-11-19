//go:build linux

package audio

import (
	"fmt"
	"log/slog"
	"strings"
	"syscall"
	"time"
)

// playbackUsesPortAudio says whether the speaker's stream is PortAudio's: not
// on Linux, where it is ALSA's, so a re-initialization of PortAudio leaves it
// playing. Tests set it to exercise both.
var playbackUsesPortAudio = false

// alsaPeriod is the ALSA period the speaker asks for: one outputFrame, which
// the Output writes at a time.
const alsaPeriod = outputFrame

// alsaBuffer is how much audio the speaker's ALSA PCM holds, eight periods: a
// write that finds less than one period free waits, so the mixer may be late
// by seven periods before the device runs out and clicks. Under the load of
// TestALSAOutputKeepsUpUnderLoad the mixer was late by up to 55 ms, and a
// 40 ms buffer ran out in 2 of 15 runs (INFRA-4592 report). The buffer does
// not slow a Cancel, which drops it (alsaStream.Drop); it delays only a sound
// queued while the stream already plays, by up to its length.
const alsaBuffer = 8 * alsaPeriod

// alsaRates are the rates the speaker tries, in order: it plays at the first
// the device takes without resampling, its native rate.
var alsaRates = []int{48000, 44100, 32000, 24000, 16000}

// alsaPCM is an ALSA playback PCM: the device's (cgoPCM) or a test's. Each
// call returns what the ALSA call it stands for returns: a frame count, 0, or
// a negative errno.
type alsaPCM interface {
	writei(interleaved []int16, frames int) int // snd_pcm_writei
	recover(rc int) int                         // snd_pcm_recover, silently
	avail() int                                 // snd_pcm_avail
	drop() int                                  // snd_pcm_drop
	prepare() int                               // snd_pcm_prepare
	close() int                                 // snd_pcm_close
}

// alsaStream is the speaker's PlaybackStream on an ALSA playback PCM that
// holds bufferFrames frames of channels interleaved channels. It writes the
// Output's mono frames to every channel.
type alsaStream struct {
	pcm          alsaPCM
	channels     int
	bufferFrames int
	wide         []int16 // a frame copied to every channel
	xruns        int
}

func newALSAStream(pcm alsaPCM, channels, bufferFrames int) *alsaStream {
	return &alsaStream{pcm: pcm, channels: channels, bufferFrames: bufferFrames}
}

// Write plays frame, waiting while the PCM is full. After an underrun, when
// the PCM ran out of audio and stopped, it prepares the PCM and plays on, and
// counts and logs the underrun: each is a click in the room.
func (s *alsaStream) Write(frame []int16) error {
	samples := frame
	if s.channels > 1 {
		if cap(s.wide) < len(frame)*s.channels {
			s.wide = make([]int16, len(frame)*s.channels)
		}
		samples = s.wide[:len(frame)*s.channels]
		for i, v := range frame {
			for c := range s.channels {
				samples[i*s.channels+c] = v
			}
		}
	}
	for frames := len(frame); frames > 0; {
		rc := s.pcm.writei(samples, frames)
		if rc >= 0 {
			samples, frames = samples[rc*s.channels:], frames-rc
			continue
		}
		if rc == -int(syscall.EPIPE) {
			if s.xruns++; s.xruns == 1 || s.xruns%100 == 0 {
				slog.Warn("speaker underrun: the device ran out of audio", "underruns", s.xruns)
			}
		}
		if s.pcm.recover(rc) < 0 {
			return alsaErr("write", rc)
		}
	}
	return nil
}

// Drop discards what the PCM holds and has not played, and prepares it for
// the next write, which starts it again once the buffer is full.
func (s *alsaStream) Drop() (int, error) {
	held := 0
	if avail := s.pcm.avail(); avail >= 0 {
		held = max(0, s.bufferFrames-avail)
	}
	if rc := s.pcm.drop(); rc < 0 {
		return 0, alsaErr("drop", rc)
	}
	if rc := s.pcm.prepare(); rc < 0 {
		return 0, alsaErr("prepare", rc)
	}
	return held, nil
}

// Close closes the PCM, dropping what it holds: the Output closes it only
// after IdleClose of silence.
func (s *alsaStream) Close() error {
	if s.xruns > 0 {
		slog.Warn("speaker stream closed after underruns", "underruns", s.xruns)
	}
	if rc := s.pcm.close(); rc < 0 {
		return alsaErr("close", rc)
	}
	return nil
}

func (s *alsaStream) underruns() int { return s.xruns }

func alsaErr(what string, rc int) error {
	return fmt.Errorf("alsa: %s: %w", what, syscall.Errno(-rc))
}

// openSpeakerStream opens the ALSA playback PCM that deviceName, the
// speaker_device setting, names (playbackDevice), and nothing else, at its
// native rate, with alsaPeriod and alsaBuffer.
func openSpeakerStream(deviceName string) (PlaybackStream, int, error) {
	device, cards := playbackDevice(deviceName)
	pcm, params, err := openALSAPlayback(device, alsaRates, alsaPeriod, alsaBuffer)
	if err != nil {
		if device == deviceName && len(cards) > 0 {
			return nil, 0, fmt.Errorf("%w: speaker_device %q is no ALSA PCM, and no sound card with a playback device matches it (cards: %s)", err, deviceName, strings.Join(cards, ", "))
		}
		return nil, 0, err
	}
	slog.Debug("speaker stream open", "device", device, "rate", params.rate, "channels", params.channels,
		"period", frameTime(params.period, params.rate), "buffer", frameTime(params.buffer, params.rate))
	return newALSAStream(pcm, params.channels, params.buffer), params.rate, nil
}

func frameTime(frames, rate int) time.Duration {
	return time.Duration(frames) * time.Second / time.Duration(rate)
}
