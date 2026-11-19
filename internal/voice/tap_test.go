package voice

import (
	"encoding/binary"
	"testing"
	"time"
)

// pcmBytes is n samples of PCM16 LE, each sample its index plus base.
func pcmBytes(n int, base int16) []byte {
	b := make([]byte, 2*n)
	for i := range n {
		binary.LittleEndian.PutUint16(b[2*i:], uint16(base+int16(i%1000)))
	}
	return b
}

// TestTapPassesNothingWithoutAnExchange fails if the tap delivers audio
// while no exchange has it open.
func TestTapPassesNothingWithoutAnExchange(t *testing.T) {
	tap := NewTap()
	tap.SetRate(16000)
	if _, err := tap.Write(pcmBytes(4000, 0)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	frames := tap.Open(0)
	if _, err := tap.Write(pcmBytes(4000, 0)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	f := <-frames
	if f.Rate != 16000 || f.Channels != 1 || len(f.PCM) != 4000 {
		t.Fatalf("frame %d Hz, %d ch, %d samples; want 16000 Hz mono, 4000 samples", f.Rate, f.Channels, len(f.PCM))
	}
	select {
	case extra := <-frames:
		t.Fatalf("an extra frame of %d samples: audio from before Open passed", len(extra.PCM))
	default:
	}
	tap.CloseOut()
	if _, ok := <-frames; ok {
		t.Fatal("frames still open after CloseOut")
	}
	if _, err := tap.Write(pcmBytes(4000, 0)); err != nil {
		t.Fatalf("Write after CloseOut: %v", err)
	}
}

// TestTapNeverBlocksAndCountsDrops fails if a full tap blocks the capture
// gate's writer, or drops without counting.
func TestTapNeverBlocksAndCountsDrops(t *testing.T) {
	tap := NewTap()
	tap.SetRate(16000)
	tap.Open(0) // nobody reads
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 4 * tapQueue {
			if _, err := tap.Write(pcmBytes(4000, 0)); err != nil {
				t.Errorf("Write: %v", err)
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(testWait):
		t.Fatal("a full tap blocked its writer")
	}
	if got := tap.Dropped(); got != uint64(3*tapQueue) {
		t.Fatalf("dropped %d frames, want %d", got, 3*tapQueue)
	}
}

// TestTapResamplesToSixteenKilohertz fails if a microphone at a fallback
// rate reaches the voice at any rate but 16 kHz.
func TestTapResamplesToSixteenKilohertz(t *testing.T) {
	tap := NewTap()
	tap.SetRate(48000)
	frames := tap.Open(0)
	if _, err := tap.Write(pcmBytes(12000, 0)); err != nil { // 250 ms
		t.Fatal(err)
	}
	f := <-frames
	if f.Rate != 16000 || len(f.PCM) != 4000 {
		t.Fatalf("frame %d Hz with %d samples, want 16000 Hz with 4000", f.Rate, len(f.PCM))
	}
}

// TestTapReplayStartsNoEarlierThanAsked fails if Open replays more of the
// ring than asked, or more than 2 s.
func TestTapReplayStartsNoEarlierThanAsked(t *testing.T) {
	tap := NewTap()
	tap.SetRate(16000)
	for i := range 12 { // 3 s, each buffer marked by its first sample
		if _, err := tap.Write(pcmBytes(4000, int16(i*1000))); err != nil {
			t.Fatal(err)
		}
	}
	frames := tap.Open(1200 * time.Millisecond)
	total := 0
	var first int16
	for len(frames) > 0 {
		f := <-frames
		if total == 0 {
			first = f.PCM[0]
		}
		total += len(f.PCM)
	}
	if total != 19200 {
		t.Fatalf("replayed %d samples, want 19200 (1.2 s)", total)
	}
	// 1.2 s back from the end of the last buffer (sample 48000) is sample
	// 28800: buffer 7, 800 samples in.
	if want := int16(7*1000 + 800); first != want {
		t.Fatalf("replay starts at sample value %d, want %d", first, want)
	}

	tap.CloseOut()
	frames = tap.Open(10 * time.Second)
	total = 0
	for len(frames) > 0 {
		total += len((<-frames).PCM)
	}
	if total != 32000 {
		t.Fatalf("replayed %d samples for 10 s asked, want the 2 s ring (32000)", total)
	}
}

// TestTapResetEmptiesTheRing fails if audio from before a gate closing can
// be replayed after it.
func TestTapResetEmptiesTheRing(t *testing.T) {
	tap := NewTap()
	tap.SetRate(16000)
	if _, err := tap.Write(pcmBytes(16000, 0)); err != nil {
		t.Fatal(err)
	}
	tap.Reset()
	frames := tap.Open(2 * time.Second)
	if n := len(frames); n != 0 {
		t.Fatalf("%d frames replayed after Reset, want none", n)
	}
}
