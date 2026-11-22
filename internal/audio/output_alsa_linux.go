//go:build linux

package audio

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
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
	resume() int                                // snd_pcm_resume
	avail() int                                 // snd_pcm_avail
	delay() (frames, rc int)                    // snd_pcm_delay
	rewindable() int                            // snd_pcm_rewindable
	rewind(frames int) int                      // snd_pcm_rewind
	status() pcmStatus                          // one snd_pcm_status snapshot
	// dropAfterStatus reads one status snapshot, then calls snd_pcm_drop,
	// and reports the snapshot, the time between the two calls, and the
	// drop's result: a status that fails drops nothing.
	dropAfterStatus() (st pcmStatus, between time.Duration, dropRC int)
	prepare() int // snd_pcm_prepare
	close() int   // snd_pcm_close
}

// pcmStatus is one snapshot of a PCM's status: whether it is running or
// suspended, its delay, which counts what is in flight past its buffer as
// well as what the buffer holds but which ALSA reports only while it runs,
// how many frames its buffer has free, which it reports in every state, its
// trigger timestamp in nanoseconds, which ALSA sets at every start, stop,
// pause, suspend and resume, and the status call's result.
type pcmStatus struct {
	running, suspended bool
	delay, avail       int
	trigger            int64
	rc                 int
}

// alsaStream is the speaker's PlaybackStream on an ALSA playback PCM, opened
// with params. It writes the Output's mono frames to every channel.
type alsaStream struct {
	pcm    alsaPCM
	device string
	params alsaParams
	wide   []int16 // a frame copied to every channel
	xruns  int
	// broke counts the times the PCM's playback broke: a suspend, resumed or
	// not, a recovery, and a Drop that held audio (breaks).
	broke int
	told  bool // the stream has logged what it is
	// again is how many of the last frames the Output gave it that the PCM
	// discarded by itself, in a suspend it could not resume from, and the
	// Output is to write again (takeAgain).
	again int
	// inflight is what was in flight to the speaker past the PCM's buffer,
	// as a USB speaker's requests are, at the last write the PCM was not
	// suspended after, from one status snapshot: a suspend cancels it, and
	// the PCM's status no longer counts it then. inflightUnknown says that
	// snapshot failed. written is how many frames the PCM took since it was
	// last prepared, less what a rewind took back, and filling says that, at
	// that snapshot, the device held some of them in flight and the room had
	// heard none (inflightAtSuspend). A prepare, which leaves nothing in
	// flight, zeroes them all.
	inflight        int
	inflightUnknown bool
	written         int
	filling         bool
}

func newALSAStream(pcm alsaPCM, device string, params alsaParams) *alsaStream {
	return &alsaStream{pcm: pcm, device: device, params: params}
}

func (s *alsaStream) bufferFrames() int { return s.params.buffer }

// queuedFrames is how many frames the room has yet to hear: what the PCM
// holds and what is in flight to the speaker. When the delay cannot be read
// it counts what the PCM holds and a whole buffer more in flight, which the
// Output's history also allows for, so the stream closes no sooner than
// that could play out.
func (s *alsaStream) queuedFrames() int {
	if d, rc := s.pcm.delay(); rc >= 0 {
		return max(0, d)
	}
	held := s.params.buffer
	if avail := s.pcm.avail(); avail >= 0 {
		held = max(0, s.params.buffer-avail)
	}
	return held + s.params.buffer
}

// playPosition reads one status snapshot of the PCM, its state, its delay
// (what it was given and has not yet played, with what the driver counts in
// flight to the speaker) and its trigger timestamp, between two reads of
// CLOCK_MONOTONIC, with the wall clock read right after the first (Output's
// timeAudible and settleAudibles). A snapshot that fails, or a clock that
// cannot be read, gives none. A suspended PCM it finds is a break (breaks),
// as it is when a write finds it.
func (s *alsaStream) playPosition() (playPosition, bool) {
	before, ok := monotonicNanos()
	if !ok {
		return playPosition{}, false
	}
	wall := time.Now()
	st := s.pcm.status()
	after, ok := monotonicNanos()
	if !ok || st.rc < 0 {
		return playPosition{}, false
	}
	if st.suspended {
		s.broke++
	}
	return playPosition{running: st.running, delay: max(0, st.delay), trigger: st.trigger, before: before, after: after, wall: wall}, true
}

// breaks reports how many times the PCM's playback broke (broke): what it
// held may then play at another time than an account read before said.
func (s *alsaStream) breaks() int { return s.broke }

// monotonicNanos reads CLOCK_MONOTONIC, the clock the acceptance driver times
// the room's recording on.
func monotonicNanos() (int64, bool) {
	var ts unix.Timespec
	if unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts) != nil {
		return 0, false
	}
	return ts.Nano(), true
}

// Write plays frame, waiting while the PCM is full. After an underrun, when
// the PCM ran out of audio and stopped, it prepares the PCM and plays on, and
// counts and logs the underrun. After a suspend, as of the system, it resumes
// the PCM, which loses nothing; if it cannot, it prepares the PCM, which
// discards what the PCM held, and leaves the rest of frame unwritten: the
// Output writes both again (takeAgain).
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
			s.written += rc
			samples, frames = samples[rc*channels:], frames-rc
			continue
		}
		if rc == -int(syscall.ESTRPIPE) {
			s.broke++
			lost, err := s.resume()
			if err != nil {
				return err
			}
			if lost >= 0 {
				s.again += lost + frames
				return nil
			}
			continue
		}
		underrun := rc == -int(syscall.EPIPE)
		if underrun {
			// Every underrun is a click in the room, and the Pi's playback
			// check counts them from these lines: a reader of /proc/asound
			// can miss a short one between its polls.
			s.xruns++
			slog.Warn("speaker underrun", "device", s.device, "underruns", s.xruns,
				"buffer", frameTime(s.params.buffer, s.params.rate))
		}
		s.broke++
		if s.pcm.recover(rc) < 0 {
			return alsaErr("write", rc)
		}
		if underrun {
			s.prepared() // recovered by a prepare
		}
	}
	switch st := s.pcm.status(); {
	case st.rc < 0:
		s.inflightUnknown = true
	case st.suspended:
		// Suspended after the write: the suspend cancelled what was in
		// flight, which a suspended status no longer counts. The next
		// write's recovery counts it lost (inflightAtSuspend). The suspend is
		// a break now, though the write succeeded (Sol's review of #154 at
		// ce454880).
		s.broke++
	default:
		s.inflight, s.inflightUnknown = s.inflightOf(st), false
		s.filling = s.inflight > 0 && st.delay >= s.written
	}
	if !s.told {
		s.tell()
	}
	return nil
}

// prepared notes that the PCM was prepared, which leaves nothing in flight.
func (s *alsaStream) prepared() {
	s.inflight, s.inflightUnknown, s.written, s.filling = 0, false, 0, false
}

// inflightOf is what st says is in flight past the PCM's buffer: its delay
// less what the buffer holds, while the PCM runs; nothing before it starts.
func (s *alsaStream) inflightOf(st pcmStatus) int {
	if !st.running {
		return 0
	}
	return max(0, st.delay-(s.params.buffer-st.avail))
}

// inflightAtSuspend is what was in flight to the speaker when the system
// suspended the PCM, from st, a suspended status, which counts what the PCM
// held but no longer what was in flight. While the speaker's queue was still
// filling at the last snapshot, the device holding audio in flight and the
// room having heard none, it is all the device took since. Else it is the
// last count, as a USB speaker keeps about as much in flight while it plays.
func (s *alsaStream) inflightAtSuspend(st pcmStatus) int {
	if s.filling {
		return max(0, s.written-(s.params.buffer-st.avail))
	}
	return s.inflight
}

// resume brings a suspended PCM back. A resume loses nothing, and it reports
// -1. A PCM that cannot resume it prepares, as snd_pcm_recover would, which
// discards what the PCM held, and it reports how many frames are lost: what
// the PCM's status says it held, and what was in flight past it, which the
// suspend cancelled, as a USB speaker's driver does (inflightAtSuspend).
// When either count is unknown, as when the status fails, a PCM that cannot
// resume fails the stream, rather than play on with audio gone.
func (s *alsaStream) resume() (lost int, err error) {
	st := s.pcm.status()
	rc := s.pcm.resume()
	for rc == -int(syscall.EAGAIN) {
		time.Sleep(alsaPeriod)
		rc = s.pcm.resume()
	}
	if rc >= 0 {
		slog.Info("speaker resumed after a suspend", "device", s.device)
		return -1, nil
	}
	if st.rc < 0 {
		return 0, alsaErr("status after a suspend", st.rc)
	}
	if s.inflightUnknown {
		return 0, errors.New("speaker could not resume after a suspend, and what was in flight to it is unknown")
	}
	if rc := s.pcm.prepare(); rc < 0 {
		return 0, alsaErr("prepare after a suspend", rc)
	}
	lost = max(0, s.params.buffer-st.avail) + s.inflightAtSuspend(st)
	s.prepared()
	slog.Warn("speaker could not resume after a suspend; its queue plays again", "device", s.device, "discarded", frameTime(lost, s.params.rate))
	return lost, nil
}

// takeAgain reports how many of the last frames the Output gave the stream
// the PCM discarded by itself, for the Output to write again, and forgets
// them.
func (s *alsaStream) takeAgain() int {
	n := s.again
	s.again = 0
	return n
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
// cuts what is in flight, and prepares it for the next write: then it reports
// what the drop cut among the frames to write again, since the room will not
// hear them otherwise. That is, from one status snapshot read just before
// the drop, the delay of a running PCM less what the device played until the
// drop, or what the buffer of a PCM not yet started holds. A PCM the system
// suspended after the delay was read also lost what was in flight at the
// last write, which the drop and the prepare hide from the next write's
// recovery, so the drop counts that too; when that count is unknown the
// stream fails, rather than play on with audio gone. The OS can stop the
// process between any two calls, so a few milliseconds may repeat. When it
// cannot read the delay or the status it drops nothing, and what it could
// not take back plays out. It logs which it did: the stream's open line says
// only whether the PCM can rewind.
func (s *alsaStream) Drop() (int, error) {
	held := s.held()
	if held == 0 {
		return 0, nil
	}
	s.broke++
	back := 0
	if n := s.pcm.rewindable(); n > 0 {
		back = max(0, s.pcm.rewind(n))
		s.written -= back
	}
	tail, rc := s.pcm.delay()
	if rc < 0 {
		slog.Warn("speaker cancel could not read the delay; what it could not rewind plays out", "device", s.device, "error", alsaErr("delay", rc))
		s.logCancel("rewind", back, 0)
		return back, nil
	}
	if back > 0 && tail <= s.params.period {
		s.logCancel("rewind", back, tail)
		return back, nil
	}
	st, between, dropRC := s.pcm.dropAfterStatus()
	if st.rc < 0 {
		slog.Warn("speaker cancel could not read the status; what it could not rewind plays out", "device", s.device, "error", alsaErr("status", st.rc))
		s.logCancel("rewind", back, tail)
		return back, nil
	}
	if dropRC < 0 {
		return 0, alsaErr("drop", dropRC)
	}
	switch {
	case st.suspended:
		if s.inflightUnknown {
			return 0, errors.New("speaker was suspended in a cancel, and what was in flight to it is unknown")
		}
		back += max(0, s.params.buffer-st.avail) + s.inflightAtSuspend(st)
	case st.running:
		played := int(int64(between) * int64(s.params.rate) / int64(time.Second))
		back += max(0, st.delay-played)
	default:
		back += max(0, s.params.buffer-st.avail)
	}
	if rc := s.pcm.prepare(); rc < 0 {
		return 0, alsaErr("prepare", rc)
	}
	s.prepared()
	s.logCancel("drop", back, tail)
	return back, nil
}

// logCancel logs how a Cancel took back frames from the PCM, and what was
// left to play then, in flight or held.
func (s *alsaStream) logCancel(way string, frames, tail int) {
	slog.Info("speaker cancel", "way", way, "took_back", frameTime(frames, s.params.rate), "left", frameTime(max(tail, 0), s.params.rate))
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
