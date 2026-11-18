//go:build linux

package audio

/*
#cgo LDFLAGS: -lasound
#include <stdlib.h>
#include <alsa/asoundlib.h>

// gw_open_playback opens the PCM name for playback, blocking on writes. It
// returns the PCM and snd_pcm_open's result, so Go passes it no pointer to
// its own memory.
typedef struct { snd_pcm_t *pcm; int rc; } gw_open_result;
static gw_open_result gw_open_playback(const char *name) {
	gw_open_result r = {0};
	r.rc = snd_pcm_open(&r.pcm, name, SND_PCM_STREAM_PLAYBACK, 0);
	return r;
}
*/
import "C"

import (
	"fmt"
	"strings"
	"unsafe"
)

// playbackUsesPortAudio says whether the speaker's playback streams are
// PortAudio's: not on Linux, where they are ALSA's, so a re-initialization of
// PortAudio leaves them playing. Tests set it to exercise both.
var playbackUsesPortAudio = false

// alsaLatency is the playback buffer ALSA keeps, in microseconds.
const alsaLatency = 500000

// openPlaybackStream opens the ALSA playback PCM that deviceName, the
// speaker_device setting, names (playbackDevice), to play buf on each Write.
func openPlaybackStream(sampleRate, channels, _ int, buf []int16, deviceName string) (outputStream, error) {
	device, cards := playbackDevice(deviceName)
	out, err := openALSAOutput(device, sampleRate, channels, buf)
	if err != nil && device == deviceName && len(cards) > 0 {
		return nil, fmt.Errorf("%w: speaker_device %q is no ALSA PCM, and no sound card with a playback device matches it (cards: %s)", err, deviceName, strings.Join(cards, ", "))
	}
	return out, err
}

// alsaOutput is an ALSA playback PCM that plays the buffer it was opened
// with, as the speaker's outputStream. It opens a playback PCM alone: no
// capture device, and no PortAudio, whose initialization opens every capture
// PCM, so it needs no red light and plays while capture is paused.
type alsaOutput struct {
	pcm      *C.snd_pcm_t
	buf      []int16
	channels int
}

// openALSAOutput opens the playback PCM device at sampleRate with channels
// interleaved 16-bit channels, which ALSA converts to what the device takes
// (soft resampling), to play buf on each Write.
func openALSAOutput(device string, sampleRate, channels int, buf []int16) (outputStream, error) {
	name := C.CString(device)
	defer C.free(unsafe.Pointer(name))
	opened := C.gw_open_playback(name)
	if opened.rc < 0 {
		return nil, alsaError("open "+device, opened.rc)
	}
	pcm := opened.pcm
	if rc := C.snd_pcm_set_params(pcm, C.SND_PCM_FORMAT_S16_LE, C.SND_PCM_ACCESS_RW_INTERLEAVED,
		C.uint(channels), C.uint(sampleRate), 1, alsaLatency); rc < 0 {
		C.snd_pcm_close(pcm)
		return nil, alsaError(fmt.Sprintf("set %s to %d Hz, %d channels", device, sampleRate, channels), rc)
	}
	return &alsaOutput{pcm: pcm, buf: buf, channels: channels}, nil
}

func alsaError(what string, rc C.int) error {
	return fmt.Errorf("alsa: %s: %s", what, C.GoString(C.snd_strerror(rc)))
}

// Start prepares the PCM; the first Write starts it.
func (o *alsaOutput) Start() error {
	if rc := C.snd_pcm_prepare(o.pcm); rc < 0 {
		return alsaError("prepare", rc)
	}
	return nil
}

// Write plays the whole buffer, waiting while ALSA's buffer is full, and
// recovers once from an underrun.
func (o *alsaOutput) Write() error {
	samples := o.buf
	for len(samples) >= o.channels {
		frames := C.snd_pcm_writei(o.pcm, unsafe.Pointer(&samples[0]), C.snd_pcm_uframes_t(len(samples)/o.channels))
		if frames < 0 {
			if rc := C.snd_pcm_recover(o.pcm, C.int(frames), 1); rc < 0 {
				return alsaError("write", rc)
			}
			continue
		}
		samples = samples[int(frames)*o.channels:]
	}
	return nil
}

// Stop plays what ALSA still holds, then stops.
func (o *alsaOutput) Stop() error {
	if rc := C.snd_pcm_drain(o.pcm); rc < 0 {
		return alsaError("drain", rc)
	}
	return nil
}

// Close closes the PCM, dropping what it still holds.
func (o *alsaOutput) Close() error {
	if o.pcm == nil {
		return nil
	}
	rc := C.snd_pcm_close(o.pcm)
	o.pcm = nil
	if rc < 0 {
		return alsaError("close", rc)
	}
	return nil
}
