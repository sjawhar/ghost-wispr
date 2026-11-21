//go:build linux

package audio

import "testing"

// TestATestBinaryRefusesEveryRealALSADevice fails if, in a test binary, an
// ALSA capture or playback open of a PCM other than ALSA's null PCM is not
// refused and recorded, or the null PCM, which is no device, is refused. The
// PCMs refused are ones ALSA does not define, so a broken guard opens nothing.
func TestATestBinaryRefusesEveryRealALSADevice(t *testing.T) {
	checkRefused(t, []refusedOpen{
		{"ALSA capture gw-no-such-pcm", func() error {
			_, err := OpenALSACapture("gw-no-such-pcm", 16000)
			return err
		}},
		{"ALSA playback gw-no-such-pcm", func() error {
			_, _, err := openALSAPlayback("gw-no-such-pcm", alsaRates, alsaPeriod, alsaBuffer)
			return err
		}},
	})
	pcm, _, err := openALSAPlayback(alsaNullPCM, alsaRates, alsaPeriod, alsaBuffer)
	if err != nil {
		t.Fatalf("ALSA's null PCM for playback = %v, want it opened: it is no device", err)
	}
	pcm.close()
}
