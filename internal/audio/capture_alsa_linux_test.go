//go:build linux

package audio

import "testing"

// TestALSACaptureReadsWholeBuffers fails if the paused listener's ALSA capture
// cannot open a PCM for 16 kHz mono capture, returns less than a buffer from a
// read, or keeps reading once closed. ALSA's "null" PCM, which every ALSA
// install defines, captures silence, so no sound card is needed.
func TestALSACaptureReadsWholeBuffers(t *testing.T) {
	capture, err := OpenALSACapture("null", 16000)
	if err != nil {
		t.Fatalf("open ALSA's null PCM for capture: %v", err)
	}
	buf := make([]int16, 1600)
	for range 3 {
		n, err := capture.Read(buf)
		if err != nil || n != len(buf) {
			t.Fatalf("read = %d, %v; want %d samples", n, err, len(buf))
		}
	}
	if err := capture.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := capture.Read(buf); err == nil {
		t.Fatal("a closed capture still read")
	}
	if _, err := OpenALSACapture("no-such-pcm-gw", 16000); err == nil {
		t.Fatal("control: a PCM ALSA does not define opened")
	}
}
