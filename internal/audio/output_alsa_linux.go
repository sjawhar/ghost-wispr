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
	delay() (frames, rc int)                    // snd_pcm_delay
	rewindable() int                            // snd_pcm_rewindable
	rewind(frames int) int                      // snd_pcm_rewind
	drop() int                                  // snd_pcm_drop
	prepare() int                               // snd_pcm_prepare
	close() int                                 // snd_pcm_close
}

// alsaStream is the speaker's PlaybackStream on an ALSA playback PCM, opened
// with params. It writes the Output's mono frames to every channel.
type alsaStream struct {
	pcm    alsaPCM
	device string
	params alsaParams
	wide   []int16 // a frame copied to every channel
	xruns  int
	told   bool // the stream has logged what it is
}

func newALSAStream(pcm alsaPCM, device string, params alsaParams) *alsaStream {
	return &alsaStream{pcm: pcm, device: device, params: params}
}

func (s *alsaStream) bufferFrames() int { return s.params.buffer }

// queuedFrames is how many frames the room has yet to hear: what the PCM
// holds and what is in flight to the speaker.
func (s *alsaStream) queuedFrames() int {
	if d, rc := s.pcm.delay(); rc >= 0 {
		return max(0, d)
	}
	return s.held()
}

// Write plays frame, waiting while the PCM is full. After an underrun, when
// the PCM ran out of audio and stopped, it prepares the PCM and plays on, and
// counts and logs the underrun: each is a click in the room.
func (s *alsaStream) Write(frame []int16) error {
	channels := s.params.channels
	samples := frame
	if channels > 1 {
		if cap(s.wide) < len(frame)*channels {
			s.wide = make([]int16, len(frame)*channels)
		}
		samples = s.wide[:len(frame)*channels]
		for i, v := range frame {
			for c := range channels {
				samples[i*channels+c] = v
			}
		}
	}
	for frames := len(frame); frames > 0; {
		rc := s.pcm.writei(samples, frames)
		if rc >= 0 {
			samples, frames = samples[rc*channels:], frames-rc
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
	if !s.told {
		s.tell()
	}
	return nil
}

// tell logs, once, what the device granted and whether a Cancel there
// rewinds or drops, now that the PCM holds audio, as the first write leaves
// it.
func (s *alsaStream) tell() {
	s.told = true
	rewinds := false
	if avail := s.pcm.avail(); avail >= 0 && avail < s.params.buffer {
		rewinds = s.pcm.rewindable() > 0
	}
	p := s.params
	slog.Info("speaker stream", "device", s.device, "rate", p.rate, "channels", p.channels,
		"period", frameTime(p.period, p.rate), "buffer", frameTime(p.buffer, p.rate), "rewind", rewinds)
}

// Drop takes back what the PCM holds and the room has not heard, and reports
// how many frames that was, for the Output to write again. It rewinds the
// PCM, so the stream plays on and ALSA reports exactly the frames it took
// back. What the device has already taken, as what is in flight to a USB
// speaker, it cannot take back, and the room hears it; when that is more than
// a period, or the PCM does not rewind, it drops the PCM instead, which also
// cuts what is in flight, and prepares it for the next write.
func (s *alsaStream) Drop() (int, error) {
	held := s.held()
	if held == 0 {
		return 0, nil
	}
	back := 0
	if n := s.pcm.rewindable(); n > 0 {
		back = max(0, s.pcm.rewind(n))
	}
	if back > 0 {
		if tail, rc := s.pcm.delay(); rc >= 0 && tail <= s.params.period {
			return back, nil
		}
	}
	back += s.held()
	if rc := s.pcm.drop(); rc < 0 {
		return 0, alsaErr("drop", rc)
	}
	if rc := s.pcm.prepare(); rc < 0 {
		return 0, alsaErr("prepare", rc)
	}
	return back, nil
}

// held is how many frames the PCM holds that the device has not taken.
func (s *alsaStream) held() int {
	if avail := s.pcm.avail(); avail >= 0 {
		return max(0, s.params.buffer-avail)
	}
	return 0
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
	return newALSAStream(pcm, device, params), params.rate, nil
}

func frameTime(frames, rate int) time.Duration {
	return time.Duration(frames) * time.Second / time.Duration(rate)
}
