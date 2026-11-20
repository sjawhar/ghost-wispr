//go:build linux

package audio

/*
#cgo LDFLAGS: -lasound
#include <errno.h>
#include <alsa/asoundlib.h>

// gw_capture is a capture PCM gw_open_capture opened, or the step that failed
// and its result.
typedef struct {
	snd_pcm_t *pcm;
	int rc;
	const char *step;
} gw_capture;

// gw_open_capture opens the PCM name for capture, blocking on reads, with
// interleaved 16-bit mono samples at rate, which a plug PCM converts from
// what the card takes, a period of period_us and a buffer of four periods.
static gw_capture gw_open_capture(const char *name, unsigned int rate, unsigned int period_us) {
	gw_capture r = {0};
	snd_pcm_hw_params_t *hw;
	snd_pcm_uframes_t period, buffer;
	snd_pcm_hw_params_alloca(&hw);
	r.step = "open";
	if ((r.rc = snd_pcm_open(&r.pcm, name, SND_PCM_STREAM_CAPTURE, 0)) < 0) {
		r.pcm = NULL;
		return r;
	}
	r.step = "read the hardware parameters of";
	if ((r.rc = snd_pcm_hw_params_any(r.pcm, hw)) < 0) goto fail;
	r.step = "set interleaved access on";
	if ((r.rc = snd_pcm_hw_params_set_access(r.pcm, hw, SND_PCM_ACCESS_RW_INTERLEAVED)) < 0) goto fail;
	r.step = "set 16-bit samples on";
	if ((r.rc = snd_pcm_hw_params_set_format(r.pcm, hw, SND_PCM_FORMAT_S16_LE)) < 0) goto fail;
	r.step = "set one channel on";
	if ((r.rc = snd_pcm_hw_params_set_channels(r.pcm, hw, 1)) < 0) goto fail;
	r.step = "set the rate of";
	if ((r.rc = snd_pcm_hw_params_set_rate(r.pcm, hw, rate, 0)) < 0) goto fail;
	period = (snd_pcm_uframes_t)rate * period_us / 1000000;
	r.step = "set the period of";
	if ((r.rc = snd_pcm_hw_params_set_period_size_near(r.pcm, hw, &period, 0)) < 0) goto fail;
	buffer = 4 * period;
	r.step = "set the buffer of";
	if ((r.rc = snd_pcm_hw_params_set_buffer_size_near(r.pcm, hw, &buffer)) < 0) goto fail;
	r.step = "apply the hardware parameters to";
	if ((r.rc = snd_pcm_hw_params(r.pcm, hw)) < 0) goto fail;
	return r;
fail:
	snd_pcm_close(r.pcm);
	r.pcm = NULL;
	return r;
}

// gw_read reads up to frames samples into buf, and recovers once from an
// overrun, which drops what the card captured meanwhile.
static snd_pcm_sframes_t gw_read(snd_pcm_t *pcm, short *buf, snd_pcm_uframes_t frames) {
	snd_pcm_sframes_t n = snd_pcm_readi(pcm, buf, frames);
	if (n == -EPIPE || n == -ESTRPIPE) {
		if (snd_pcm_recover(pcm, (int)n, 1) < 0) return n;
		n = snd_pcm_readi(pcm, buf, frames);
	}
	return n;
}
*/
import "C"

import (
	"errors"
	"fmt"
	"time"
	"unsafe"
)

// ALSACapture is a capture PCM opened through ALSA alone, in Ghost Wispr's
// own process: no PortAudio, whose initialization opens every capture device,
// and no outside process. The paused listener reads the microphone with it.
// Read and Close belong to one goroutine.
type ALSACapture struct{ pcm *C.snd_pcm_t }

// ALSACapturePeriod is how much audio one Read waits for.
const ALSACapturePeriod = 100 * time.Millisecond

// OpenALSACapture opens device, an ALSA PCM name (CaptureDevice), for 16-bit
// mono capture at rate.
func OpenALSACapture(device string, rate int) (*ALSACapture, error) {
	name := C.CString(device)
	defer C.free(unsafe.Pointer(name))
	r := C.gw_open_capture(name, C.uint(rate), C.uint(ALSACapturePeriod/time.Microsecond))
	if r.pcm == nil {
		return nil, fmt.Errorf("ALSA: %s %s for capture: %s", C.GoString(r.step), device, C.GoString(C.snd_strerror(r.rc)))
	}
	return &ALSACapture{pcm: r.pcm}, nil
}

// Read fills pcm with samples, blocking until it has them.
func (c *ALSACapture) Read(pcm []int16) (int, error) {
	if c.pcm == nil {
		return 0, errors.New("ALSA capture closed")
	}
	if len(pcm) == 0 {
		return 0, nil
	}
	n := C.gw_read(c.pcm, (*C.short)(unsafe.Pointer(&pcm[0])), C.snd_pcm_uframes_t(len(pcm)))
	if n < 0 {
		return 0, fmt.Errorf("ALSA capture read: %s", C.GoString(C.snd_strerror(C.int(n))))
	}
	return int(n), nil
}

// Close closes the PCM, which releases the device.
func (c *ALSACapture) Close() error {
	if c.pcm == nil {
		return nil
	}
	rc := C.snd_pcm_close(c.pcm)
	c.pcm = nil
	if rc < 0 {
		return fmt.Errorf("ALSA capture close: %s", C.GoString(C.snd_strerror(rc)))
	}
	return nil
}
