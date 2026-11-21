package audio

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// Reproductions of four speaker defects that GPT-6.1 Sol found in review of
// #133, at b6eef5c, and reproduced on main's speaker: a Cancel during a
// replay, Drain before the resampler's tail, take-back accounting across a
// silence, and a second Cancel erasing the first one's cut. They drive the
// real Output, Playback, mixer and fakeStream; each test runs its passing
// control before its case.

type probeStream struct {
	*fakeStream
	// Only the Output mixer accesses these fields after Open is released.
	writes int
	before func(int, []int16)
	after  func(int, []int16)
	drop   func() (int, error)
}

func (s *probeStream) Write(frame []int16) error {
	s.writes++
	n := s.writes
	if s.before != nil {
		s.before(n, frame)
	}
	err := s.fakeStream.Write(frame)
	if s.after != nil {
		s.after(n, frame)
	}
	return err
}

func (s *probeStream) Drop() (int, error) {
	if s.drop != nil {
		return s.drop()
	}
	return s.fakeStream.Drop()
}

type probeBarrier struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newProbeBarrier() *probeBarrier {
	return &probeBarrier{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (b *probeBarrier) hold() {
	close(b.entered)
	<-b.release
}

func (b *probeBarrier) unblock() {
	b.once.Do(func() { close(b.release) })
}

func probeWait(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("fixture: timed out waiting for %s", what)
	}
}

// Opening is gated so every hook and initial playback is installed before
// the mixer runs. Cleanup opens every gate before waiting for Output.Close.
func probeOutput(t *testing.T, stream PlaybackStream) (*Output, func(), <-chan SpeechEvent) {
	t.Helper()
	return probeOutputAt(t, stream, 48000)
}

// probeOutputAt is probeOutput with a stream that runs at rate.
func probeOutputAt(t *testing.T, stream PlaybackStream, rate int) (*Output, func(), <-chan SpeechEvent) {
	t.Helper()
	open := make(chan struct{})
	var once sync.Once
	start := func() { once.Do(func() { close(open) }) }
	out := NewOutput(OutputConfig{
		Open: func(string) (PlaybackStream, int, error) {
			<-open
			return stream, rate, nil
		},
	})
	t.Cleanup(func() {
		if err := out.Close(); err != nil {
			t.Errorf("fixture: Output.Close: %v", err)
		}
	})
	t.Cleanup(start)
	events := make(chan SpeechEvent, 64)
	remove := out.OnSpeech(func(e SpeechEvent) { events <- e })
	t.Cleanup(remove)
	return out, start, events
}

func probeVoiceEvent(t *testing.T, events <-chan SpeechEvent, started bool) SpeechEvent {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case e := <-events:
			if e.Input == InputVoice && e.Started == started {
				return e
			}
		case <-timer.C:
			t.Fatalf("fixture: no voice event with Started=%t", started)
			return SpeechEvent{}
		}
	}
}

func probeNonzero(frame []int16) int {
	n := 0
	for _, sample := range frame {
		if sample != 0 {
			n++
		}
	}
	return n
}

// TestACancelDuringAReplayStopsIt fails if a Cancel of an input that comes
// while the mixer replays what the device took back leaves the rest of the
// replay with that input's audio: 80 ms played after the cancel on main. The
// control cancels before the replay is mixed, and its replay is silent.
func TestACancelDuringAReplayStopsIt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		during bool
	}{
		{name: "control_cancel_before_replay_is_built"},
		{name: "treatment_cancel_during_first_replay_write", during: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := &probeStream{fakeStream: newFakeStream(48000)}
			out, start, events := probeOutput(t, stream)
			barrier := newProbeBarrier()
			t.Cleanup(barrier.unblock)
			ctx := testContext(t)
			voice, chime := out.Begin(InputVoice), out.Begin(InputChime)
			if err := voice.WriteAll(ctx, constant(48000, 200*time.Millisecond, 10000), mono48k); err != nil {
				t.Fatal(err)
			}
			if err := chime.WriteAll(ctx, constant(48000, 200*time.Millisecond, -500), mono48k); err != nil {
				t.Fatal(err)
			}
			type replayResult struct {
				writes         int
				nonzeroWrites  int
				nonzeroSamples int
			}
			done := make(chan replayResult, 1)
			var replay replayResult
			drops := 0
			stream.drop = func() (int, error) {
				drops++
				if drops == 1 {
					return 8 * 480, nil
				}
				return 0, nil
			}
			stream.before = func(n int, frame []int16) {
				if n == 8 {
					out.Cancel(InputChime)
					if !tc.during {
						out.Cancel(InputVoice)
					}
				}
				if n == 9 {
					barrier.hold()
				}
			}
			stream.after = func(n int, frame []int16) {
				if n >= 9 && n <= 16 {
					replay.writes++
					nonzero := probeNonzero(frame)
					replay.nonzeroSamples += nonzero
					if nonzero != 0 {
						replay.nonzeroWrites++
					}
					if n == 16 {
						done <- replay
					}
				}
			}
			start()
			probeWait(t, barrier.entered, "first replay write")
			_ = probeVoiceEvent(t, events, true)
			if tc.during {
				out.Cancel(InputVoice)
			}
			barrier.unblock()
			var result replayResult
			select {
			case result = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("fixture: eight-frame replay did not finish")
			}
			if result.writes != 8 {
				t.Fatalf("fixture: replay writes=%d, want 8", result.writes)
			}
			t.Logf("cancel-during-replay=%t replay-writes=%d nonzero-writes-after-cancel=%d nonzero-samples-after-cancel=%d audio-after-cancel=%v", tc.during, result.writes, result.nonzeroWrites, result.nonzeroSamples, time.Duration(result.nonzeroWrites)*outputFrame)
			if !tc.during && result.nonzeroWrites != 0 {
				t.Fatalf("control: cancellation before retake left voice in %d replay writes (%d samples)", result.nonzeroWrites, result.nonzeroSamples)
			}
			if result.nonzeroWrites > 2 {
				t.Fatalf("Cancel exceeded the existing 20ms stream-time bound: %d subsequent nonzero replay writes, %d samples, %v of audio", result.nonzeroWrites, result.nonzeroSamples, time.Duration(result.nonzeroWrites)*outputFrame)
			}
		})
	}
}

// TestDrainWaitsForTheResamplersTail fails if Drain returns while the frame
// that holds the resampler's tail of its audio is not yet taken by the
// stream: on main the playback's frame number stopped at its last queued
// sample. The control's audio needs no resampler.
func TestDrainWaitsForTheResamplersTail(t *testing.T) {
	for _, tc := range []struct {
		name string
		rate int
		d    time.Duration
	}{
		{name: "control_same_rate_final_samples_in_frame_two", rate: 48000, d: 20 * time.Millisecond},
		{name: "treatment_resampler_tail_in_frame_two", rate: 24000, d: 10 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := &probeStream{fakeStream: newFakeStream(48000)}
			out, start, _ := probeOutput(t, stream)
			barrier := newProbeBarrier()
			t.Cleanup(barrier.unblock)
			ctx := testContext(t)
			pb := out.Begin(InputVoice)
			format := AudioFormat{SampleRate: tc.rate, Channels: 1, Encoding: EncodingPCM16}
			if err := pb.WriteAll(ctx, constant(tc.rate, tc.d, 1000), format); err != nil {
				t.Fatal(err)
			}
			tailSamples := make(chan int, 1)
			returned := make(chan struct{})
			stream.before = func(n int, frame []int16) {
				if n == 2 {
					tailSamples <- probeNonzero(frame)
					barrier.hold()
				}
			}
			stream.after = func(n int, _ []int16) {
				if n == 2 {
					close(returned)
				}
			}
			start()
			probeWait(t, barrier.entered, "second audio/tail write")
			nonzero := <-tailSamples
			if nonzero == 0 {
				t.Fatal("fixture: held second write contains no playback audio")
			}
			out.mu.Lock()
			pending, seq, written, idle := pb.pending, pb.seq, out.written, pb.in.idle()
			out.mu.Unlock()
			// With the final audio write still blocked, Drain must need to
			// wait. A pre-cancelled context makes that distinction immediate:
			// waiting returns context.Canceled; mistaken completion returns nil.
			stopped, cancel := context.WithCancel(context.Background())
			cancel()
			whileHeld := pb.Drain(stopped)
			barrier.unblock()
			probeWait(t, returned, "second write return")
			if err := pb.Drain(ctx); err != nil {
				t.Fatalf("Drain after successful acceptance = %v", err)
			}
			t.Logf("rate=%d held-nonzero-samples=%d pending=%d playback-seq=%d written-seq=%d input-idle=%t Drain(held,cancelled-context)=%v", tc.rate, nonzero, pending, seq, written, idle, whileHeld)
			if !errors.Is(whileHeld, context.Canceled) {
				t.Fatalf("Drain treated blocked final audio as complete: rate=%d held-nonzero-samples=%d result=%v pending=%d playback-seq=%d written-seq=%d input-idle=%t; want context.Canceled while waiting", tc.rate, nonzero, whileHeld, pending, seq, written, idle)
			}
		})
	}
}

// TestDrainWaitsForItsTailWhenAnotherPlaybackFollows fails if Drain returns
// while the frame that holds its resampler's tail is not yet taken by the
// stream, when a later playback on the same input queued audio that shares
// that frame: the frame was counted for the later playback only (Sol's review
// of #142 at ef951b3), or, with the first playback ending so that its last
// sample's delayed output falls on a frame's last position, for the frame
// that sample reaches, though the filter's response to it runs into the next
// one (Sol's review at 7da737b: 224 samples at 24 kHz). The later playback is
// silence, so every audio sample of the held frame is the first's tail. The
// control has no later playback. The device runs at 48 kHz, and at 44.1 kHz.
func TestDrainWaitsForItsTailWhenAnotherPlaybackFollows(t *testing.T) {
	for _, tc := range []struct {
		device, samples int
		next            bool
	}{
		{48000, 240, false},
		{48000, 240, true},
		{48000, 224, true},
		{44100, 224, true},
	} {
		t.Run(fmt.Sprintf("device %d Hz, %d samples, a later playback queued %v", tc.device, tc.samples, tc.next), func(t *testing.T) {
			stream := &probeStream{fakeStream: newFakeStream(tc.device)}
			gate := make(chan struct{})
			out := NewOutput(OutputConfig{Open: func(string) (PlaybackStream, int, error) {
				<-gate
				return stream, tc.device, nil
			}})
			open := sync.OnceFunc(func() { close(gate) })
			barrier := newProbeBarrier()
			t.Cleanup(func() {
				barrier.unblock()
				open()
				_ = out.Close()
			})
			ctx := testContext(t)
			format := AudioFormat{SampleRate: 24000, Channels: 1, Encoding: EncodingPCM16}
			signal := make([]int16, tc.samples)
			for i := range signal {
				signal[i] = 10000
			}
			first := out.Begin(InputVoice)
			if err := first.WriteAll(ctx, pcm16Bytes(signal), format); err != nil {
				t.Fatal(err)
			}
			if tc.next {
				if err := out.Begin(InputVoice).WriteAll(ctx, constant(24000, 10*time.Millisecond, 0), format); err != nil {
					t.Fatal(err)
				}
			}
			tail := make(chan int, 1)
			stream.before = func(n int, frame []int16) {
				if n == 2 {
					tail <- probeNonzero(frame)
					barrier.hold()
				}
			}
			open()
			probeWait(t, barrier.entered, "the second write, which holds the first playback's tail")
			samples := <-tail
			if samples == 0 {
				t.Fatal("fixture: the held second write holds none of the first playback's audio")
			}
			stopped, cancel := context.WithCancel(context.Background())
			cancel()
			whileHeld := first.Drain(stopped)
			barrier.unblock()
			if err := first.Drain(ctx); err != nil {
				t.Fatalf("Drain once the tail was taken = %v", err)
			}
			if !errors.Is(whileHeld, context.Canceled) {
				t.Fatalf("Drain returned %v while %d samples of its tail waited in the held write; want context.Canceled while it waits", whileHeld, samples)
			}
		})
	}
}

// TestAPlaybacksFrameIsTheLastItsAudioReaches fails, at any of ten input
// rates on six device rates and ending at several positions in a frame, if
// a playback's Drain could return once a frame is written while a later
// frame still holds its audio, or if an input that runs out drops part of the
// filter's response to its last audio. Sol's reviews of #142 found 350 such
// playbacks at 7da737b, then, at dd08536, a Drain that returned between the
// write of one frame and the mix of the next, and an input that ended 29
// samples of response early. The answer comes from the same audio followed by
// a later playback of silence, which keeps the filter running, so that a
// truncated end cannot define it: the audio alone must sound the same, and at
// each frame boundary Drain's own condition (Playback.Drain) is checked. A
// playback's frame may come one frame after the last that holds audio a
// sample rounds to: the filter's far edge is below one step of a sample.
func TestAPlaybacksFrameIsTheLastItsAudioReaches(t *testing.T) {
	type run struct {
		pcm     []int16
		frames  uint64
		drainAt uint64 // the first frame whose write would let Drain return; 0 is none
		seq     uint64
	}
	render := func(from, to, m int, next bool) run {
		in := &outputInput{gen: &playGen{}, tails: make([]tail, 0, 4)}
		in.setRate(from)
		in.contrib, in.landed, in.landedBy = make([]float32, to/100), make([]uint32, 2*to/100), make([]owners, 2*to/100)
		a := &Playback{}
		signal := make([]int16, m)
		for i := range signal {
			signal[i] = 10000
		}
		in.push(a, pcm16Bytes(signal), 1, m)
		if next {
			n := from/100 + 100
			in.push(&Playback{}, make([]byte, 2*n), 1, n)
		}
		var r run
		for seq := uint64(1); !in.idle(); seq++ {
			if seq > 40 {
				t.Fatal("fixture: the input never ran out")
			}
			in.seq = seq
			in.render(to)
			clear(in.landed) // the mixer moves these on
			for _, v := range in.contrib {
				r.pcm = append(r.pcm, clip(v))
			}
			r.frames = seq
			if r.drainAt == 0 && a.pending == 0 && (in.last != a || in.idle()) && a.seq <= seq {
				r.drainAt = seq
			}
		}
		r.seq = a.seq
		return r
	}
	failures := 0
	fail := func(format string, args ...any) {
		if failures++; failures <= 8 {
			t.Errorf(format, args...)
		}
	}
	total := 0
	for _, from := range []int{8000, 16000, 22050, 24000, 32000, 44100, 48000, 96000, 192000, 384000} {
		for _, to := range []int{8000, 16000, 24000, 44100, 48000, 96000} {
			for delta := -33; delta <= 1; delta++ {
				m := from/100 + delta
				ref := render(from, to, m, true)
				lastAudio := uint64(0)
				for i, v := range ref.pcm {
					if v != 0 {
						lastAudio = uint64(i/(to/100)) + 1
					}
				}
				for _, next := range []bool{false, true} {
					total++
					r := ref
					if !next {
						r = render(from, to, m, false)
						lost := 0
						for i, v := range ref.pcm {
							if i >= len(r.pcm) && v != 0 || i < len(r.pcm) && r.pcm[i] != v {
								lost++
							}
						}
						if lost > 0 {
							fail("%d Hz on %d Hz, %d samples: alone, %d samples of its sound differ from the same audio with silence after it", from, to, m, lost)
						}
					}
					if r.drainAt != 0 && r.drainAt < lastAudio {
						fail("%d Hz on %d Hz, %d samples, a later playback %v: Drain could return once frame %d was written, and frame %d holds its audio", from, to, m, next, r.drainAt, lastAudio)
					}
					if r.seq > lastAudio+1 {
						fail("%d Hz on %d Hz, %d samples, a later playback %v: the playback's frame is %d, its audio ends in frame %d", from, to, m, next, r.seq, lastAudio)
					}
				}
			}
		}
	}
	if failures > 0 {
		t.Errorf("%d failures in %d playbacks", failures, total)
	}
}

// pacedHeldStream is a playback stream whose writes return at once, as
// ALSA's null PCM does, so the mixer's pacer holds it to real time; its
// twelfth write waits until release closes, having sent how many samples of
// it hold audio on entered.
type pacedHeldStream struct {
	writes  int // only the mixer touches it
	entered chan int
	release chan struct{}
}

func (s *pacedHeldStream) Write(frame []int16) error {
	if s.writes++; s.writes == 12 {
		s.entered <- probeNonzero(frame)
		<-s.release
	}
	return nil
}

func (*pacedHeldStream) Drop() (int, error) { return 0, nil }
func (*pacedHeldStream) Close() error       { return nil }

// TestDrainWaitsThroughThePacersWait fails if a playback's Drain returns
// while the mixer's pacer waits between two frames, with the next frame,
// which the stream holds, still holding the filter's response to its audio:
// the playback was marked for that frame only once its mix began (Sol's
// review of #142 at dd08536). A later playback of silence keeps the filter
// running. The control's audio needs no resampler, and its Drain returns.
func TestDrainWaitsThroughThePacersWait(t *testing.T) {
	for _, tc := range []struct{ rate, ending int }{{48000, 240}, {24000, 224}, {24000, 239}} {
		t.Run(fmt.Sprintf("%d Hz, ending %d samples into a frame", tc.rate, tc.ending), func(t *testing.T) {
			stream := &pacedHeldStream{entered: make(chan int, 1), release: make(chan struct{})}
			gate := make(chan struct{})
			out := NewOutput(OutputConfig{Open: func(string) (PlaybackStream, int, error) {
				<-gate
				return stream, 48000, nil
			}})
			open, release := sync.OnceFunc(func() { close(gate) }), sync.OnceFunc(func() { close(stream.release) })
			t.Cleanup(func() {
				release()
				open()
				_ = out.Close()
			})
			ctx := testContext(t)
			format := AudioFormat{SampleRate: tc.rate, Channels: 1, Encoding: EncodingPCM16}
			signal := make([]int16, tc.rate/10+tc.ending)
			for i := range signal {
				signal[i] = 10000
			}
			a := out.Begin(InputVoice)
			if err := a.WriteAll(ctx, pcm16Bytes(signal), format); err != nil {
				t.Fatal(err)
			}
			if err := out.Begin(InputVoice).WriteAll(ctx, make([]byte, 480), format); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- a.Drain(ctx) }()
			for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(time.Millisecond) {
				out.mu.Lock()
				waiting := len(out.waiting) > 0
				out.mu.Unlock()
				if waiting {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("control: Drain never waited")
				}
			}
			open()
			var held int
			select {
			case held = <-stream.entered:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			select {
			case err := <-done:
				if held > 0 {
					t.Fatalf("Drain returned %v while the held twelfth write holds %d samples of its audio", err, held)
				}
			case <-time.After(100 * time.Millisecond):
				if held == 0 {
					t.Fatal("control: Drain did not return, and the held write holds none of its audio")
				}
			}
		})
	}
}

// TestATakeBackOfResampledAudioCountsItsQueuedTime fails if a Cancel's
// take-back of a resampled burst subtracts more than the burst's own time
// from what its playback played: it counted the stream positions its output
// spans, the resampler's delay included, at the device's rate, and took part
// of an earlier burst that was heard off (Sol's review of #142 at 7da737b:
// 9.333334 ms left of 10 ms). A 10 ms burst plays in the first two writes;
// after a gap in the same speech, another plays in writes 6 and 7, and a
// Cancel during write 7 takes back writes 5 to 7. The control's audio is at
// the device's rate.
func TestATakeBackOfResampledAudioCountsItsQueuedTime(t *testing.T) {
	for _, rate := range []int{48000, 24000} {
		t.Run(fmt.Sprintf("input %d Hz", rate), func(t *testing.T) {
			stream := &probeStream{fakeStream: newFakeStream(48000)}
			out, start, events := probeOutput(t, stream)
			ctx := testContext(t)
			p := out.Begin(InputVoice)
			format := AudioFormat{SampleRate: rate, Channels: 1, Encoding: EncodingPCM16}
			burst := constant(rate, 10*time.Millisecond, 10000)
			if err := p.WriteAll(ctx, burst, format); err != nil {
				t.Fatal(err)
			}
			before := make(chan time.Duration, 1)
			stream.before = func(n int, _ []int16) {
				switch n {
				case 5:
					before <- p.Played()
					if err := p.Write(ctx, burst, format); err != nil {
						t.Error(err)
					}
				case 7:
					out.Cancel(InputVoice)
				}
			}
			stream.drop = func() (int, error) { return 3 * 480, nil }
			start()
			probeVoiceEvent(t, events, true)
			ended := probeVoiceEvent(t, events, false)
			first := <-before
			if first != 10*time.Millisecond {
				t.Fatalf("control: the first burst played %v, want 10ms", first)
			}
			if got := p.Played(); got != first || ended.Played != first {
				t.Fatalf("after the second burst was taken back, the playback played %v and its speech %v; want the first burst's %v", got, ended.Played, first)
			}
		})
	}
}

// TestATakeBackCountsOnlyTheSpeechsAudio fails if a Cancel's take-back
// subtracts silence from what a speech played: on main it took the whole span
// from the speech's first audio to its last as audio, gaps included. The
// control's audio has no gap.
func TestATakeBackCountsOnlyTheSpeechsAudio(t *testing.T) {
	for _, tc := range []struct {
		name string
		gap  bool
	}{
		{name: "control_contiguous_audio"},
		{name: "treatment_gap_inside_one_speech", gap: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := &probeStream{fakeStream: newFakeStream(48000)}
			out, start, events := probeOutput(t, stream)
			ctx := testContext(t)
			pb := out.Begin(InputVoice)
			initial := 40 * time.Millisecond
			cancelWrite := 4
			if tc.gap {
				initial = 10 * time.Millisecond
				cancelWrite = 6
			}
			if err := pb.WriteAll(ctx, constant(48000, initial, 1000), mono48k); err != nil {
				t.Fatal(err)
			}
			type queuedAgain struct {
				wasSpeaking bool
				err         error
			}
			queued := make(chan queuedAgain, 1)
			type dropResult struct {
				droppedFrames  int
				survivingAudio int
			}
			dropped := make(chan dropResult, 1)
			drops := 0
			stream.before = func(n int, _ []int16) {
				if tc.gap && n == 5 {
					out.mu.Lock()
					wasSpeaking := pb.in.speaking
					out.mu.Unlock()
					err := pb.Write(ctx, constant(48000, 10*time.Millisecond, 1000), mono48k)
					queued <- queuedAgain{wasSpeaking: wasSpeaking, err: err}
				}
				if n == cancelWrite {
					out.Cancel(InputVoice)
				}
			}
			stream.drop = func() (int, error) {
				drops++
				if drops != 1 {
					return 0, nil
				}
				const back = 3 * 480
				all := stream.samples()
				if len(all) < back {
					dropped <- dropResult{droppedFrames: back, survivingAudio: -1}
					return 0, nil
				}
				// Count actual samples outside the device suffix being taken
				// back, independently of Output's span/accounting fields.
				dropped <- dropResult{droppedFrames: back, survivingAudio: probeNonzero(all[:len(all)-back])}
				return back, nil
			}
			start()
			_ = probeVoiceEvent(t, events, true)
			ended := probeVoiceEvent(t, events, false)
			if tc.gap {
				select {
				case q := <-queued:
					if q.err != nil || !q.wasSpeaking {
						t.Fatalf("fixture: second burst did not join the first speech: was-speaking=%t write-error=%v first-end=%+v", q.wasSpeaking, q.err, ended)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("fixture: second burst was never queued")
				}
			}
			var result dropResult
			select {
			case result = <-dropped:
			case <-time.After(5 * time.Second):
				t.Fatalf("fixture: no take-back; first end=%+v", ended)
			}
			if result.survivingAudio != 480 {
				t.Fatalf("fixture: surviving nonzero samples=%d, want 480 (10ms); drop=%d", result.survivingAudio, result.droppedFrames)
			}
			want := time.Duration(result.survivingAudio) * time.Second / 48000
			played := pb.Played()
			t.Logf("gap=%t surviving-nonzero-samples=%d dropped-frames=%d expected-played=%v playback-played=%v event-played=%v cancelled=%t", tc.gap, result.survivingAudio, result.droppedFrames, want, played, ended.Played, ended.Cancelled)
			if !ended.Cancelled || played != want || ended.Played != want {
				t.Fatalf("take-back counted silence as speech: gap=%t surviving-nonzero-samples=%d want-played=%v playback-played=%v event-played=%v cancelled=%t", tc.gap, result.survivingAudio, want, played, ended.Played, ended.Cancelled)
			}
		})
	}
}

// TestASecondCancelKeepsTheFirstCut fails if a second Cancel, before the
// mixer takes the first in, erases the first one's cut, so the speech's end
// says it was not cancelled. The control cancels once.
func TestASecondCancelKeepsTheFirstCut(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cancels int
	}{
		{name: "control_one_cancel", cancels: 1},
		{name: "treatment_two_cancels_before_retake", cancels: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := &probeStream{fakeStream: newFakeStream(48000)}
			out, start, events := probeOutput(t, stream)
			barrier := newProbeBarrier()
			t.Cleanup(barrier.unblock)
			ctx := testContext(t)
			pb := out.Begin(InputVoice)
			if err := pb.WriteAll(ctx, constant(48000, 80*time.Millisecond, 1000), mono48k); err != nil {
				t.Fatal(err)
			}
			stream.before = func(n int, _ []int16) {
				if n == 2 {
					barrier.hold()
				}
			}
			// The inherited fakeStream.Drop returns zero, just as the real
			// PortAudio implementation does: queued audio is still cut.
			start()
			probeWait(t, barrier.entered, "second ordinary write")
			_ = probeVoiceEvent(t, events, true)
			out.mu.Lock()
			queuedBefore := pb.in.n
			out.mu.Unlock()
			if queuedBefore == 0 {
				t.Fatal("fixture: no queued audio remains for Cancel to cut")
			}
			var firstCut, finalCut bool
			for i := range tc.cancels {
				out.Cancel(InputVoice)
				out.mu.Lock()
				cut := pb.in.cut
				out.mu.Unlock()
				if i == 0 {
					firstCut = cut
				}
				finalCut = cut
			}
			if err := pb.Write(ctx, constant(48000, 10*time.Millisecond, 1000), mono48k); !errors.Is(err, ErrCancelled) {
				t.Fatalf("fixture: cancelled playback accepted more audio: %v", err)
			}
			barrier.unblock()
			ended := probeVoiceEvent(t, events, false)
			t.Logf("cancels=%d queued-samples-discarded=%d first-cut=%t final-cut=%t end-cancelled=%t played=%v", tc.cancels, queuedBefore, firstCut, finalCut, ended.Cancelled, ended.Played)
			if !firstCut {
				t.Fatal("fixture: first Cancel failed to observe queued speech")
			}
			if !ended.Cancelled {
				t.Fatalf("second Cancel erased the first queued cut: cancels=%d queued-samples-discarded=%d first-cut=%t final-cut=%t end=%+v", tc.cancels, queuedBefore, firstCut, finalCut, ended)
			}
		})
	}
}

// TestATakeBackComesOffEachPlaybackItTook fails if a Cancel whose Drop takes
// back audio of two playbacks of one input takes it off only the one the
// stream took last: the first still reported 10 ms played, though the room
// heard none of it (Sol's review of #133 at aaa8e0f). Played sizes what the
// text-to-speech voice reports it spoke. The control is one playback of the
// same 20 ms.
func TestATakeBackComesOffEachPlaybackItTook(t *testing.T) {
	for _, split := range []bool{false, true} {
		t.Run(fmt.Sprintf("two playbacks %v", split), func(t *testing.T) {
			stream := &probeStream{fakeStream: newFakeStream(48000)}
			out, start, events := probeOutput(t, stream)
			barrier := newProbeBarrier()
			t.Cleanup(barrier.unblock)
			ctx := testContext(t)
			first := out.Begin(InputVoice)
			second := first
			if err := first.Write(ctx, constant(48000, 10*time.Millisecond, 1000), mono48k); err != nil {
				t.Fatal(err)
			}
			if split {
				second = out.Begin(InputVoice)
			}
			if err := second.Write(ctx, constant(48000, 10*time.Millisecond, 1000), mono48k); err != nil {
				t.Fatal(err)
			}
			stream.before = func(n int, _ []int16) {
				if n == 3 { // the first silent write after both
					barrier.hold()
				}
			}
			drops := 0
			stream.drop = func() (int, error) {
				if drops++; drops == 1 {
					return 3 * 480, nil // all of both, and the silence after
				}
				return 0, nil
			}
			start()
			probeWait(t, barrier.entered, "the first silent write after both playbacks")
			probeVoiceEvent(t, events, true)
			out.Cancel(InputVoice)
			barrier.unblock()
			end := probeVoiceEvent(t, events, false)
			if end.Played != 0 {
				t.Fatalf("fixture: the speech played %v, though its audio was all taken back", end.Played)
			}
			if a, b := first.Played(), second.Played(); a != 0 || b != 0 {
				t.Fatalf("taken-back audio still counts as played: the first playback %v, the second %v", a, b)
			}
		})
	}
}

// TestATakeBackSplitsAPositionTwoPlaybacksShare fails if a stream position
// that holds samples of two playbacks, as one 48 kHz position of a 96 kHz
// input can hold the end of one and the start of the next, charges the whole
// take-back of it to the later one: the first, all of it taken back, still
// reported 10.416 µs played (Sol's review of #148 at fd1d2b6). The control
// puts the boundary on a resampling step, where no position is shared.
func TestATakeBackSplitsAPositionTwoPlaybacksShare(t *testing.T) {
	for _, firstFrames := range []int{2, 1} {
		t.Run(fmt.Sprintf("the first playback %d samples at 96 kHz", firstFrames), func(t *testing.T) {
			stream := &probeStream{fakeStream: newFakeStream(48000)}
			out, start, events := probeOutput(t, stream)
			barrier := newProbeBarrier()
			t.Cleanup(barrier.unblock)
			ctx := testContext(t)
			first, second := out.Begin(InputVoice), out.Begin(InputVoice)
			format := AudioFormat{SampleRate: 96000, Channels: 1, Encoding: EncodingPCM16}
			if err := first.Write(ctx, pcm16Bytes(make([]int16, firstFrames)), format); err != nil {
				t.Fatal(err)
			}
			if err := second.Write(ctx, pcm16Bytes(make([]int16, 960-firstFrames)), format); err != nil {
				t.Fatal(err)
			}
			stream.before = func(n int, _ []int16) {
				if n == 3 {
					barrier.hold()
				}
			}
			stream.drop = func() (int, error) { return 3 * 480, nil }
			start()
			probeWait(t, barrier.entered, "all the resampled output queued")
			if first.Played() == 0 || second.Played() == 0 {
				t.Fatalf("control: before the take-back the playbacks played %v and %v", first.Played(), second.Played())
			}
			out.Cancel(InputVoice)
			barrier.unblock()
			end := probeVoiceEvent(t, events, false)
			if a, b := first.Played(), second.Played(); a != 0 || b != 0 {
				t.Fatalf("all of it taken back, the first playback played %v and the second %v (the speech %v)", a, b, end.Played)
			}
		})
	}
}

// TestAThirdPlaybackInAPositionTakesNothingOffTheFirst fails if a stream
// position that holds the end of one playback, all of a second shorter than
// a resampling step, and the start of a third charges the second's samples
// to the first when a Cancel takes the position back: the first then lost
// part of its audio that played, 9.984 ms reported of its 10 ms heard, where
// main kept its 10 ms (Sol's review of #148 at 2e03d9d). The first keeps
// exactly the audio outside the take-back, and the third, all of it taken
// back, none; the second keeps its samples as played, as on main. The
// control has no second playback. Two rates: 384 kHz on a 48 kHz device,
// and 48 kHz on a 16 kHz one.
func TestAThirdPlaybackInAPositionTakesNothingOffTheFirst(t *testing.T) {
	for _, tc := range []struct {
		input, device, first, middle, rest, back int
	}{
		{input: 384000, device: 48000, first: 3841, middle: 6, rest: 3839, back: 1424},
		{input: 48000, device: 16000, first: 481, middle: 1, rest: 479, back: 464},
	} {
		for _, middle := range []int{0, tc.middle} {
			t.Run(fmt.Sprintf("%d Hz on %d Hz, %d samples between", tc.input, tc.device, middle), func(t *testing.T) {
				stream := &probeStream{fakeStream: newFakeStream(tc.device)}
				out, start, events := probeOutputAt(t, stream, tc.device)
				ctx := testContext(t)
				format := AudioFormat{SampleRate: tc.input, Channels: 1, Encoding: EncodingPCM16}
				pbs := []*Playback{out.Begin(InputVoice), out.Begin(InputVoice), out.Begin(InputVoice)}
				for i, n := range []int{tc.first, middle, tc.rest - middle} {
					pcm := make([]int16, n)
					for j := range pcm {
						pcm[j] = 1000
					}
					if err := pbs[i].Write(ctx, pcm16Bytes(pcm), format); err != nil {
						t.Fatal(err)
					}
				}
				stream.before = func(n int, _ []int16) {
					if n == 4 {
						out.Cancel(InputVoice)
					}
				}
				stream.drop = func() (int, error) { return tc.back, nil }
				start()
				end := probeVoiceEvent(t, events, false)
				if end.Played != 10*time.Millisecond {
					t.Fatalf("fixture: the speech played %v, want the first playback's 10 ms", end.Played)
				}
				if a, c := pbs[0].Played(), pbs[2].Played(); a != 10*time.Millisecond || c != 0 {
					t.Fatalf("the first playback played %v, want the 10 ms outside the take-back; the third %v, want none (the second %v)", a, c, pbs[1].Played())
				}
			})
		}
	}
}

// TestAPlaybackBackInAPositionLosesOnlyItsOwnSamples fails if a stream
// position that holds the end of a playback, all of a second shorter than a
// resampling step, and one more sample of the first, written after the
// second, takes the second's samples off the first when a Cancel takes the
// position back: the first then lost part of its audio that played, 9.984
// ms reported of its 10 ms heard, where main kept its 10 ms (Sol's review of
// #148 at db8c2a0). The first keeps exactly the audio outside the take-back,
// and the last, all of it taken back, none. The control writes the second
// nothing.
func TestAPlaybackBackInAPositionLosesOnlyItsOwnSamples(t *testing.T) {
	for _, middle := range []int{0, 6} {
		t.Run(fmt.Sprintf("%d samples between", middle), func(t *testing.T) {
			stream := &probeStream{fakeStream: newFakeStream(48000)}
			out, start, events := probeOutput(t, stream)
			ctx := testContext(t)
			a, b, d := out.Begin(InputVoice), out.Begin(InputVoice), out.Begin(InputVoice)
			format := AudioFormat{SampleRate: 384000, Channels: 1, Encoding: EncodingPCM16}
			for _, part := range []struct {
				pb *Playback
				n  int
			}{{a, 3841}, {b, middle}, {a, 1}, {d, 3838 - middle}} {
				pcm := make([]int16, part.n)
				for i := range pcm {
					pcm[i] = 1000
				}
				if err := part.pb.Write(ctx, pcm16Bytes(pcm), format); err != nil {
					t.Fatal(err)
				}
			}
			stream.before = func(n int, _ []int16) {
				if n == 4 {
					out.Cancel(InputVoice)
				}
			}
			stream.drop = func() (int, error) { return 1424, nil }
			start()
			end := probeVoiceEvent(t, events, false)
			if end.Played != 10*time.Millisecond {
				t.Fatalf("fixture: the speech played %v, want the 10 ms before the take-back", end.Played)
			}
			if got, last := a.Played(), d.Played(); got != 10*time.Millisecond || last != 0 {
				t.Fatalf("the first playback played %v, want the 10 ms outside the take-back; the last %v, want none (the one between %v)", got, last, b.Played())
			}
		})
	}
}

// TestAReplayOfAResampledTailStampsTheSpeechsEnd fails if a speech of 24 kHz
// audio on a 48 kHz stream, whose filter tail a Drop of another input brings
// back, keeps an end stamped before that replay is taken: the speech's audio
// was taken to end where its last queued sample lands, before its filter
// response ends, so a replay starting after that point was not its speech
// (Sol's review of #133 at aaa8e0f: 18 voice samples replayed, the end
// 160 ms before them). The 480-sample control's replay includes the point
// where the last sample lands.
func TestAReplayOfAResampledTailStampsTheSpeechsEnd(t *testing.T) {
	for _, back := range []int{480, 440} {
		t.Run(fmt.Sprintf("%d samples given back", back), func(t *testing.T) {
			stream := &probeStream{fakeStream: newFakeStream(48000)}
			out, start, events := probeOutput(t, stream)
			barrier := newProbeBarrier()
			t.Cleanup(barrier.unblock)
			ctx := testContext(t)
			voice, chime := out.Begin(InputVoice), out.Begin(InputChime)
			if err := voice.Write(ctx, constant(24000, 10*time.Millisecond, 10000), AudioFormat{SampleRate: 24000, Channels: 1}); err != nil {
				t.Fatal(err)
			}
			if err := chime.Write(ctx, constant(48000, 20*time.Millisecond, 7), mono48k); err != nil {
				t.Fatal(err)
			}
			replayed, taken := make(chan int, 1), make(chan time.Time, 1)
			drops := 0
			stream.drop = func() (int, error) {
				if drops++; drops == 1 {
					return back, nil
				}
				return 0, nil
			}
			stream.before = func(n int, frame []int16) {
				switch n {
				case 2:
					out.Cancel(InputChime)
				case 3: // the replay
					replayed <- probeNonzero(frame)
					barrier.hold()
				}
			}
			stream.after = func(n int, _ []int16) {
				if n == 3 {
					taken <- time.Now()
				}
			}
			start()
			probeWait(t, barrier.entered, "the replay of the voice's filter tail")
			samples := <-replayed
			if samples == 0 {
				t.Fatal("fixture: the replay holds none of the voice")
			}
			time.Sleep(150 * time.Millisecond)
			barrier.unblock()
			at := <-taken
			end := probeVoiceEvent(t, events, false)
			if end.Cancelled {
				t.Fatal("fixture: the voice was cancelled")
			}
			if end.At.Before(at) {
				t.Fatalf("the speech ended %v before the stream took the replay of %d samples of it", at.Sub(end.At).Round(time.Millisecond), samples)
			}
		})
	}
}

// underrunAfterFirstWrite is a stream whose first write sends when it
// returned, and that reports an underrun from then on, so the Output calls
// its Underrun callback right after that write and before it records it.
type underrunAfterFirstWrite struct {
	writes int // only the mixer touches it
	first  chan time.Time
}

func (s *underrunAfterFirstWrite) Write([]int16) error {
	if s.writes++; s.writes == 1 {
		s.first <- time.Now()
	}
	return nil
}

func (s *underrunAfterFirstWrite) underruns() int   { return min(s.writes, 1) }
func (*underrunAfterFirstWrite) Drop() (int, error) { return 0, nil }
func (*underrunAfterFirstWrite) Close() error       { return nil }

// TestAStartTheStreamTookBeforeACancelIsReported fails if a speech whose
// first audio the stream took reports no start, and so no end either, when a
// Cancel came after the stream took it but before the mixer recorded that:
// the speech was taken for one Cancel cut while the stream held its audio
// (Sol's review of #133 at 4238ad3). The Underrun callback, called between
// the write's return and its record, holds the mixer there, and the cut end
// must follow. The control has no Cancel, and its speech starts and ends.
func TestAStartTheStreamTookBeforeACancelIsReported(t *testing.T) {
	for _, early := range []bool{false, true} {
		t.Run(fmt.Sprintf("the Cancel before the record %v", early), func(t *testing.T) {
			stream := &underrunAfterFirstWrite{first: make(chan time.Time, 1)}
			barrier := newProbeBarrier()
			out := NewOutput(OutputConfig{Open: func(string) (PlaybackStream, int, error) { return stream, 48000, nil }, Underrun: barrier.hold})
			t.Cleanup(func() { _ = out.Close() })
			t.Cleanup(barrier.unblock)
			events := make(chan SpeechEvent, 16)
			defer out.OnSpeech(func(e SpeechEvent) { events <- e })()
			pb := out.Begin(InputVoice)
			// 100 ms, all queued before the stream takes the first frame, so a
			// Cancel while the mixer is held cuts audio still queued.
			ctx := testContext(t)
			if err := pb.WriteAll(ctx, constant(48000, 100*time.Millisecond, 1000), mono48k); err != nil {
				t.Fatal(err)
			}
			probeWait(t, barrier.entered, "the Underrun callback after the first write returned")
			took := <-stream.first
			if early {
				out.Cancel(InputVoice)
			}
			barrier.unblock()
			var first SpeechEvent
			select {
			case first = <-events:
			case <-time.After(time.Second):
				t.Fatal("the speech reported no start, though the stream took its first audio before the Cancel")
			}
			if !first.Started || first.At.Before(took) {
				t.Fatalf("the first event is %+v, want a start at or after %v", first, took)
			}
			end := probeVoiceEvent(t, events, false)
			if end.Cancelled != early {
				t.Fatalf("the end is %+v, want cancelled %v", end, early)
			}
		})
	}
}

// TestAFilterFlushOfSilenceDoesNotMoveTheSpeechsEnd fails if a speech of
// 24 kHz audio on a 48 kHz stream, whose filter flush reaches a frame that
// rounds entirely to silence, is stamped ended when the stream took that
// silent frame: the speech's end moved past its last audio by however long
// the device held that write (Sol's review of #133 at 4238ad3: 161 ms with
// 209 samples). The 240-sample control's flush frame holds audible samples,
// so the end is that frame's. Either way the end comes after the stream took
// the last audible write and before the first silent one began.
func TestAFilterFlushOfSilenceDoesNotMoveTheSpeechsEnd(t *testing.T) {
	for _, n := range []int{240, 209} {
		t.Run(fmt.Sprintf("%d samples", n), func(t *testing.T) {
			s := &ordinaryEndStream{held: make(chan struct{}), release: make(chan struct{})}
			gate := make(chan struct{})
			out := NewOutput(OutputConfig{Open: func(string) (PlaybackStream, int, error) {
				<-gate
				return s, 48000, nil
			}})
			open, release := sync.OnceFunc(func() { close(gate) }), sync.OnceFunc(func() { close(s.release) })
			t.Cleanup(func() {
				release()
				open()
				_ = out.Close()
			})
			events := make(chan SpeechEvent, 4)
			defer out.OnSpeech(func(e SpeechEvent) { events <- e })()
			signal := make([]int16, n)
			for i := range signal {
				signal[i] = 10000
			}
			ctx := testContext(t)
			if err := out.Begin(InputVoice).Write(ctx, pcm16Bytes(signal), AudioFormat{SampleRate: 24000, Channels: 1, Encoding: EncodingPCM16}); err != nil {
				t.Fatal(err)
			}
			open()
			probeWait(t, s.held, "the first write that is all silence")
			time.Sleep(150 * time.Millisecond)
			release()
			probeVoiceEvent(t, events, true)
			end := probeVoiceEvent(t, events, false)
			s.mu.Lock()
			lastVoice, silent := s.lastVoice, s.firstSilent
			s.mu.Unlock()
			if lastVoice.IsZero() || !silent.After(lastVoice) || end.Cancelled {
				t.Fatalf("control: the last audible write returned at %v, the first silent one began at %v, the end %+v", lastVoice, silent, end)
			}
			if end.At.Before(lastVoice) || !end.At.Before(silent) {
				t.Fatalf("the speech ended %v after its last audible write, %v after the first silent write began", end.At.Sub(lastVoice).Round(time.Millisecond), end.At.Sub(silent).Round(time.Millisecond))
			}
		})
	}
}

// TestAPartialTakeBackCountsTheDelayedSampleCentres fails if a Cancel whose
// take-back covers only the second of two frames takes off what that frame
// held at no delay, not the queued samples whose output it holds after the
// resampler's delay: at 24 kHz the second frame holds the last 224 of the
// 240 samples' centres (Sol's review of #133 at 4238ad3 found the take-back
// test giving back all of a speech, so it could not tell). The control's
// audio is at the stream's rate, where the second frame holds the last
// 10 ms.
func TestAPartialTakeBackCountsTheDelayedSampleCentres(t *testing.T) {
	for _, rate := range []int{48000, 24000} {
		t.Run(fmt.Sprintf("input %d Hz", rate), func(t *testing.T) {
			o := &Output{inputs: map[string]*outputInput{}, wake: make(chan struct{}, 1), eventWake: make(chan struct{}, 1), listeners: map[int]func(SpeechEvent){}}
			p := o.Begin(InputVoice)
			ctx := testContext(t)
			if err := p.Write(ctx, constant(rate, 10*time.Millisecond, 10000), AudioFormat{SampleRate: rate, Channels: 1}); err != nil {
				t.Fatal(err)
			}
			frame, sum := make([]int16, 480), make([]float32, 480)
			for range 2 {
				o.mixFrame(frame, sum, 48000, 4800, time.Now())
				o.wrote(o.mixed, time.Now())
			}
			if got := p.Played(); got != 10*time.Millisecond {
				t.Fatalf("control: before the take-back the playback played %v", got)
			}
			o.Cancel(InputVoice)
			o.retake(make([]int16, 480), sum) // the second frame only
			want := 10 * time.Millisecond
			if rate == 24000 {
				want = 224 * time.Second / 24000
			}
			if got := p.Played(); got < want-time.Nanosecond || got > want+time.Nanosecond {
				t.Fatalf("after taking back the second frame the playback played %v, want %v", got, want)
			}
		})
	}
}
