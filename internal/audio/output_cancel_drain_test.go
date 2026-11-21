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
	open := make(chan struct{})
	var once sync.Once
	start := func() { once.Do(func() { close(open) }) }
	out := NewOutput(OutputConfig{
		Open: func(string) (PlaybackStream, int, error) {
			<-open
			return stream, 48000, nil
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
		in.contrib, in.landed = make([]float32, to/100), make([]uint32, 2*to/100)
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
