//go:build linux

package audio

/*
#cgo LDFLAGS: -lasound
#include <stdlib.h>
#include <alsa/asoundlib.h>

// gw_playback is a playback PCM gw_open_playback opened, with the parameters
// it set, or the step that failed and its result.
typedef struct {
	snd_pcm_t *pcm;
	int rc;
	const char *step;
	unsigned int rate, channels;
	snd_pcm_uframes_t period, buffer;
} gw_playback;

// gw_open_playback opens the PCM name for playback, blocking on writes, with
// interleaved 16-bit samples on as few channels as it takes, at the first of
// rates it takes without resampling (else the nearest to the first), a
// period of period_us and a buffer of buffer_us. It starts once the buffer is
// full and wakes a write once a period is free. Go passes it no pointer to
// its own memory but rates, which holds none.
static gw_playback gw_open_playback(const char *name, const unsigned int *rates, int nrates,
                                    unsigned int period_us, unsigned int buffer_us) {
	gw_playback r = {0};
	snd_pcm_hw_params_t *hw;
	snd_pcm_sw_params_t *sw;
	int i;
	snd_pcm_hw_params_alloca(&hw);
	snd_pcm_sw_params_alloca(&sw);
	r.step = "open";
	if ((r.rc = snd_pcm_open(&r.pcm, name, SND_PCM_STREAM_PLAYBACK, 0)) < 0) {
		r.pcm = NULL;
		return r;
	}
	r.step = "read the hardware parameters of";
	if ((r.rc = snd_pcm_hw_params_any(r.pcm, hw)) < 0) goto fail;
	r.step = "turn off resampling on";
	if ((r.rc = snd_pcm_hw_params_set_rate_resample(r.pcm, hw, 0)) < 0) goto fail;
	r.step = "set interleaved access on";
	if ((r.rc = snd_pcm_hw_params_set_access(r.pcm, hw, SND_PCM_ACCESS_RW_INTERLEAVED)) < 0) goto fail;
	r.step = "set 16-bit samples on";
	if ((r.rc = snd_pcm_hw_params_set_format(r.pcm, hw, SND_PCM_FORMAT_S16_LE)) < 0) goto fail;
	r.channels = 1;
	r.step = "set the channels of";
	if ((r.rc = snd_pcm_hw_params_set_channels_near(r.pcm, hw, &r.channels)) < 0) goto fail;
	r.rate = 0;
	for (i = 0; i < nrates && r.rate == 0; i++) {
		if (snd_pcm_hw_params_test_rate(r.pcm, hw, rates[i], 0) == 0) r.rate = rates[i];
	}
	r.step = "set the rate of";
	if (r.rate != 0) {
		r.rc = snd_pcm_hw_params_set_rate(r.pcm, hw, r.rate, 0);
	} else {
		r.rate = rates[0];
		r.rc = snd_pcm_hw_params_set_rate_near(r.pcm, hw, &r.rate, 0);
	}
	if (r.rc < 0) goto fail;
	r.period = (snd_pcm_uframes_t)r.rate * period_us / 1000000;
	r.step = "set the period of";
	if ((r.rc = snd_pcm_hw_params_set_period_size_near(r.pcm, hw, &r.period, 0)) < 0) goto fail;
	r.buffer = (snd_pcm_uframes_t)r.rate * buffer_us / 1000000;
	r.step = "set the buffer of";
	if ((r.rc = snd_pcm_hw_params_set_buffer_size_near(r.pcm, hw, &r.buffer)) < 0) goto fail;
	r.step = "apply the hardware parameters to";
	if ((r.rc = snd_pcm_hw_params(r.pcm, hw)) < 0) goto fail;
	r.step = "read the software parameters of";
	if ((r.rc = snd_pcm_sw_params_current(r.pcm, sw)) < 0) goto fail;
	r.step = "set the start threshold of";
	if ((r.rc = snd_pcm_sw_params_set_start_threshold(r.pcm, sw, r.buffer)) < 0) goto fail;
	r.step = "set the wake threshold of";
	if ((r.rc = snd_pcm_sw_params_set_avail_min(r.pcm, sw, r.period)) < 0) goto fail;
	r.step = "apply the software parameters to";
	if ((r.rc = snd_pcm_sw_params(r.pcm, sw)) < 0) goto fail;
	return r;
fail:
	snd_pcm_close(r.pcm);
	r.pcm = NULL;
	return r;
}
*/
import "C"

import (
	"fmt"
	"time"
	"unsafe"
)

// cgoPCM is an ALSA playback PCM on a device.
type cgoPCM struct{ pcm *C.snd_pcm_t }

func (p *cgoPCM) writei(interleaved []int16, frames int) int {
	return int(C.snd_pcm_writei(p.pcm, unsafe.Pointer(&interleaved[0]), C.snd_pcm_uframes_t(frames)))
}

func (p *cgoPCM) recover(rc int) int { return int(C.snd_pcm_recover(p.pcm, C.int(rc), 1)) }
func (p *cgoPCM) avail() int         { return int(C.snd_pcm_avail(p.pcm)) }
func (p *cgoPCM) drop() int          { return int(C.snd_pcm_drop(p.pcm)) }
func (p *cgoPCM) prepare() int       { return int(C.snd_pcm_prepare(p.pcm)) }
func (p *cgoPCM) rewindable() int    { return int(C.snd_pcm_rewindable(p.pcm)) }

func (p *cgoPCM) rewind(frames int) int {
	return int(C.snd_pcm_rewind(p.pcm, C.snd_pcm_uframes_t(frames)))
}

func (p *cgoPCM) delay() (frames, rc int) {
	var d C.snd_pcm_sframes_t
	rc = int(C.snd_pcm_delay(p.pcm, &d))
	return int(d), rc
}

func (p *cgoPCM) close() int {
	if p.pcm == nil {
		return 0
	}
	rc := C.snd_pcm_close(p.pcm)
	p.pcm = nil
	return int(rc)
}

// alsaParams are the parameters a playback PCM opened with, in frames.
type alsaParams struct{ rate, channels, period, buffer int }

// alsaNullPCM is ALSA's null PCM, which discards what it plays and captures
// silence: no device, so a test binary opens it (realDevice).
const alsaNullPCM = "null"

// openALSAPlayback opens the playback PCM device alone: no capture device,
// and no PortAudio, whose initialization opens every capture PCM. It plays
// at the first of rates the device takes without resampling, with interleaved
// 16-bit channels, which ALSA converts to what the device takes. In a test
// binary it opens only ALSA's null PCM (realDevice).
func openALSAPlayback(device string, rates []int, period, buffer time.Duration) (*cgoPCM, alsaParams, error) {
	if device != alsaNullPCM {
		if err := realDevice("ALSA playback " + device); err != nil {
			return nil, alsaParams{}, err
		}
	}
	name := C.CString(device)
	defer C.free(unsafe.Pointer(name))
	crates := make([]C.uint, len(rates))
	for i, r := range rates {
		crates[i] = C.uint(r)
	}
	r := C.gw_open_playback(name, &crates[0], C.int(len(crates)), C.uint(period/time.Microsecond), C.uint(buffer/time.Microsecond))
	if r.rc < 0 {
		return nil, alsaParams{}, fmt.Errorf("alsa: %s %s: %s", C.GoString(r.step), device, C.GoString(C.snd_strerror(r.rc)))
	}
	return &cgoPCM{pcm: r.pcm}, alsaParams{rate: int(r.rate), channels: int(r.channels), period: int(r.period), buffer: int(r.buffer)}, nil
}
