package audio

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"
)

// Inputs the Output mixes. Any other name makes an input of its own.
const (
	InputVoice = "voice"
	InputChime = "chime"
)

// MaxWriteFrame is the longest frame Playback.Write takes.
const MaxWriteFrame = 20 * time.Millisecond

// InputQueue is the most audio one input holds: a Write that would queue more
// waits until the stream has taken some.
const InputQueue = 200 * time.Millisecond

// OutputIdleClose is how long the Output keeps its stream open with nothing
// queued on any input. The Jabra's light cannot change while a stream is open
// on it, so a resume waits for the close (captureDevice.prepareLightForOpen):
// this is how long a resume waits after the last sound.
const OutputIdleClose = 300 * time.Millisecond

// outputFrame is how much audio the Output mixes and writes to the stream at
// a time, one ALSA period. A Cancel takes effect before the next frame.
const outputFrame = 10 * time.Millisecond

// outputHistory is how much of each input's part in the stream the Output
// keeps, to mix again without a cancelled input what the stream dropped:
// more than the device holds (alsaBuffer).
const outputHistory = 100 * time.Millisecond

// outputAhead is how far the Output may run ahead of real time on a stream
// that never makes a write wait, as ALSA's null PCM. A device's stream makes
// it wait once its buffer is full, well before that.
const outputAhead = 100 * time.Millisecond

// speechEndHold is how long an input has nothing to play before its speech
// ends, so that a writer a moment late with its next frame does not end it.
const speechEndHold = 100 * time.Millisecond

// outputCloseWait is how long Close waits for the mixer to close its stream.
const outputCloseWait = 5 * time.Second

var (
	// ErrOutputClosed ends every playback once the Output is closed.
	ErrOutputClosed = errors.New("speaker output closed")
	// ErrCancelled ends a playback that Cancel, or a PortAudio teardown,
	// silenced.
	ErrCancelled = errors.New("speaker playback cancelled")
	// ErrFrameTooLong is what Write returns for a frame longer than
	// MaxWriteFrame.
	ErrFrameTooLong = errors.New("frame longer than 20 ms")
	// ErrInvalidFormat is what Write returns for audio that is not whole
	// frames of PCM16 at a positive rate and channel count.
	ErrInvalidFormat = errors.New("invalid PCM16 format")
)

// PlaybackStream is a playback stream on the speaker device: ALSA's on Linux
// (alsaStream), PortAudio's elsewhere. Only the Output's mixer calls it.
type PlaybackStream interface {
	// Write plays frame, mono PCM16 at the stream's rate, and waits while the
	// device's buffer is full.
	Write(frame []int16) error
	// Drop discards what the device holds and has not played, and reports
	// how many frames that was; the next Write plays at once. A stream that
	// cannot tell drops nothing and reports 0.
	Drop() (frames int, err error)
	// Close stops the stream, dropping what the device holds, and releases
	// the device.
	Close() error
}

// OutputConfig configures an Output.
type OutputConfig struct {
	// Device is the speaker_device setting: on Linux an ALSA PCM or a sound
	// card's id or name (playbackDevice); "" is the system default.
	Device string
	// Opening and Closing bracket each stream: Opening runs just before the
	// stream opens, and Closing once the Output is done with it, whether it
	// opened or not, and must call closeStream, which closes it. The Jabra
	// mute button uses them to keep the Jabra's light from changing while a
	// stream is open on the Jabra.
	Opening func()
	Closing func(closeStream func())
	// IdleClose is how long the stream stays open with nothing queued; 0 is
	// OutputIdleClose.
	IdleClose time.Duration
	// Open opens a stream on the device and reports its rate; nil opens the
	// speaker device (openSpeakerStream). Tests set it.
	Open func(device string) (stream PlaybackStream, rate int, err error)
}

// SpeechEvent reports that an input's audio started or ended on the stream,
// for barge-in. A speech ends once its input has had nothing to play for
// speechEndHold, or at once on Cancel.
type SpeechEvent struct {
	Input   string
	Started bool // false: the speech ended
	// At is when the speech's first audio went to the stream, or its last.
	At time.Time
	// Played, on an end, is how much of the input's audio the stream took in
	// the speech, less what a Cancel dropped from the device.
	Played    time.Duration
	Cancelled bool
}

// Output plays audio on the speaker device through one playback stream that
// mixes named inputs, such as the voice and the chime. The stream opens when
// the first frame is queued on any input, and closes once IdleClose has
// passed with nothing queued on any; Opening and Closing bracket it. Each
// input takes PCM16 at the rate its writer declares and resamples it to the
// stream's, holds at most InputQueue, and reports its speech starting and
// ending. Cancel silences an input before the next frame: it drops what the
// device holds and plays the other inputs' part of it again.
//
// On Linux the stream is the ALSA playback PCM the Device setting names, and
// nothing else: no capture device, and no PortAudio, whose initialization
// opens every capture PCM. So it needs no red recording light, and plays
// while capture is paused.
type Output struct {
	cfg OutputConfig

	mu     sync.Mutex
	inputs map[string]*outputInput
	order  []*outputInput // in the order they were made
	closed bool
	// closeNow asks the mixer to close its stream before its next frame, for
	// a PortAudio teardown (stopForTeardown) or Close.
	closeNow bool
	// dropNow asks the mixer to drop what the device holds and play it again
	// without the inputs Cancel silenced.
	dropNow bool
	// room is closed, and replaced, when the stream takes queued audio or a
	// playback ends, while a Write or Drain waits (waiters).
	room    chan struct{}
	waiters int
	// mixed counts the frames mixed, and written the last of them the
	// stream has taken: a Drain waits for its playback's last frame.
	mixed, written uint64
	// wake has a value once a Write queued audio, or on Close, for the mixer
	// waiting without a stream.
	wake chan struct{}
	done chan struct{} // closed once the mixer has returned

	listeners map[int]func(SpeechEvent)
	nextID    int
	events    []SpeechEvent
	eventWake chan struct{}
}

// outputInput is one input's queue and its part in the mix. The Output's mu
// guards it.
type outputInput struct {
	name string
	live map[*Playback]struct{} // playbacks begun and not ended

	// ring holds the queued audio, mono at rate, from head on; segments says
	// which playback each run of it belongs to, oldest first.
	ring     []int16
	head, n  int
	rate     int
	segments []segment

	rs resampler
	// flush is how many samples of silence the resampler still needs to play
	// out the last queued audio.
	flush int
	// last is the playback whose audio the stream took last.
	last *Playback
	seq  uint64 // the frame being mixed

	speaking  bool
	cancelled bool // Cancel silenced its speech: the mixer drops and ends it
	// silenced is the playback whose audio the stream took last before
	// Cancel: what the drop keeps from the room comes off what it played.
	silenced  *Playback
	lastAudio time.Time
	quietAt   time.Time // when it ran out of audio; zero while it has some
	spoken    playedTime

	contrib []float32 // its part in the frame being mixed
	history []float32 // its part in the stream's last outputHistory: a ring that ends at histPos
	histPos int
}

type segment struct {
	pb *Playback
	n  int
}

// Playback is one writer's audio on an input, such as one spoken answer. Its
// writes play in order until Cancel ends it. An input plays the audio of its
// playbacks in the order it was queued, so give an input one writer at a
// time.
type Playback struct {
	out *Output
	in  *outputInput
	// Guarded by out.mu:
	err     error  // why it ended: ErrCancelled, a stream failure, ErrOutputClosed
	pending int    // samples queued and not yet taken by the stream
	seq     uint64 // the frame that took its last sample
	played  playedTime
}

// playedTime adds up audio from its sample count at each rate, exactly.
type playedTime struct {
	done    time.Duration
	rate    int
	samples int64
}

func (p *playedTime) add(rate, samples int) {
	if rate != p.rate {
		p.done, p.rate, p.samples = p.total(), rate, 0
	}
	p.samples += int64(samples)
}

// sub takes d off, as for audio a drop kept from the room.
func (p *playedTime) sub(d time.Duration) {
	p.done, p.samples = max(0, p.total()-d), 0
}

func (p *playedTime) total() time.Duration {
	if p.rate == 0 {
		return p.done
	}
	return p.done + time.Duration(p.samples)*time.Second/time.Duration(p.rate)
}

// NewOutput makes an Output. It opens no stream until audio is queued.
func NewOutput(cfg OutputConfig) *Output {
	if cfg.Opening == nil {
		cfg.Opening = func() {}
	}
	if cfg.Closing == nil {
		cfg.Closing = func(closeStream func()) { closeStream() }
	}
	if cfg.IdleClose <= 0 {
		cfg.IdleClose = OutputIdleClose
	}
	if cfg.Open == nil {
		cfg.Open = openSpeakerStream
	}
	o := &Output{
		cfg:       cfg,
		inputs:    map[string]*outputInput{},
		room:      make(chan struct{}),
		wake:      make(chan struct{}, 1),
		done:      make(chan struct{}),
		listeners: map[int]func(SpeechEvent){},
		eventWake: make(chan struct{}, 1),
	}
	go o.run()
	go o.dispatch()
	return o
}

// Begin starts a playback on input, such as InputVoice.
func (o *Output) Begin(input string) *Playback {
	o.mu.Lock()
	defer o.mu.Unlock()
	in := o.inputs[input]
	if in == nil {
		in = &outputInput{name: input, live: map[*Playback]struct{}{}}
		o.inputs[input] = in
		o.order = append(o.order, in)
	}
	p := &Playback{out: o, in: in}
	if o.closed {
		p.err = ErrOutputClosed
	} else {
		in.live[p] = struct{}{}
	}
	return p
}

// Write queues frame, at most MaxWriteFrame of whole PCM16 frames in format,
// whose channels it mixes down to one. It waits while the input holds
// InputQueue, or holds audio at another rate. Once the playback has ended it
// returns why: ErrCancelled, the stream's failure, or ErrOutputClosed.
func (p *Playback) Write(ctx context.Context, frame []byte, format AudioFormat) error {
	frames, err := wholeFrames(frame, format)
	if err != nil {
		return err
	}
	if int64(frames)*int64(time.Second) > int64(format.SampleRate)*int64(MaxWriteFrame) {
		return fmt.Errorf("%w: %d frames at %d Hz", ErrFrameTooLong, frames, format.SampleRate)
	}
	o, in := p.out, p.in
	o.mu.Lock()
	defer o.mu.Unlock()
	for {
		if p.err != nil {
			return p.err
		}
		if frames == 0 {
			return nil
		}
		if in.rate != format.SampleRate && in.idle() {
			in.setRate(format.SampleRate)
		}
		if in.rate == format.SampleRate && in.n+frames <= len(in.ring) {
			in.push(p, frame, format.Channels, frames)
			select {
			case o.wake <- struct{}{}:
			default:
			}
			return nil
		}
		if err := o.waitRoom(ctx); err != nil {
			return err
		}
	}
}

// WriteAll queues pcm, whole PCM16 frames in format, in frames of
// MaxWriteFrame, waiting for room as Write does.
func (p *Playback) WriteAll(ctx context.Context, pcm []byte, format AudioFormat) error {
	if _, err := wholeFrames(pcm, format); err != nil {
		return err
	}
	step := max(1, format.SampleRate*int(MaxWriteFrame/time.Millisecond)/1000) * format.Channels * 2
	for off := 0; off < len(pcm); off += step {
		if err := p.Write(ctx, pcm[off:min(off+step, len(pcm))], format); err != nil {
			return err
		}
	}
	return nil
}

// wholeFrames reports how many whole frames pcm holds in format.
func wholeFrames(pcm []byte, format AudioFormat) (int, error) {
	if format.Encoding != EncodingPCM16 || format.SampleRate <= 0 || format.Channels <= 0 {
		return 0, fmt.Errorf("%w: %s at %d Hz with %d channels", ErrInvalidFormat, format.Encoding, format.SampleRate, format.Channels)
	}
	size := 2 * format.Channels
	if len(pcm)%size != 0 {
		return 0, fmt.Errorf("%w: %d bytes is no whole number of %d-byte frames", ErrInvalidFormat, len(pcm), size)
	}
	return len(pcm) / size, nil
}

// Drain waits until the stream has taken all the playback queued, or the
// playback ends, and then returns why it ended.
func (p *Playback) Drain(ctx context.Context) error {
	o, in := p.out, p.in
	o.mu.Lock()
	defer o.mu.Unlock()
	for {
		if p.err != nil {
			return p.err
		}
		if p.pending == 0 && (in.last != p || in.idle()) && o.written >= p.seq {
			return nil
		}
		if err := o.waitRoom(ctx); err != nil {
			return err
		}
	}
}

// Played reports how much of the playback's audio the stream has taken, less
// what a Cancel dropped from the device.
func (p *Playback) Played() time.Duration {
	p.out.mu.Lock()
	defer p.out.mu.Unlock()
	return p.played.total()
}

// waitRoom waits, with mu released, until the stream takes audio or a
// playback ends. The caller holds mu.
func (o *Output) waitRoom(ctx context.Context) error {
	room := o.room
	o.waiters++
	o.mu.Unlock()
	defer func() {
		o.mu.Lock()
		o.waiters--
	}()
	select {
	case <-room:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// changed wakes every Write and Drain that waits. The caller holds mu.
func (o *Output) changed() {
	if o.waiters > 0 {
		close(o.room)
		o.room = make(chan struct{})
	}
}

// Cancel silences input: it drops what the input holds, ends each of its
// playbacks with ErrCancelled, and has the mixer drop what the device holds
// and play the other inputs' part of it again, before its next frame. A
// playback begun after Cancel plays.
func (o *Output) Cancel(input string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	in := o.inputs[input]
	if in == nil {
		return
	}
	last := in.last
	in.end(ErrCancelled)
	if in.speaking {
		in.cancelled, in.silenced = true, last
		o.dropNow = true
	}
	o.changed()
}

// OnSpeech calls fn with each speech event, in order, on a goroutine of the
// Output's; remove stops it. fn must not wait on the Output.
func (o *Output) OnSpeech(fn func(SpeechEvent)) (remove func()) {
	o.mu.Lock()
	defer o.mu.Unlock()
	id := o.nextID
	o.nextID++
	o.listeners[id] = fn
	return func() {
		o.mu.Lock()
		defer o.mu.Unlock()
		delete(o.listeners, id)
	}
}

// Close ends every playback with ErrOutputClosed and closes the stream. It
// waits up to outputCloseWait for a stream write under way to return.
func (o *Output) Close() error {
	o.mu.Lock()
	if !o.closed {
		o.closed, o.closeNow = true, true
		o.endAll(ErrOutputClosed)
		signal(o.wake)
		signal(o.eventWake)
	}
	o.mu.Unlock()
	select {
	case <-o.done:
		return nil
	case <-time.After(outputCloseWait):
		return errors.New("speaker output: a stream write did not return")
	}
}

// stopForTeardown ends every playback with ErrCancelled and has the mixer
// close its stream, for a PortAudio teardown (outputGuard.stopAll).
func (o *Output) stopForTeardown() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.closeNow = true
	o.endAll(ErrCancelled)
}

// endAll ends every playback with err and every speech, cancelled. The caller
// holds mu.
func (o *Output) endAll(err error) {
	now := time.Now()
	for _, in := range o.order {
		in.end(err)
		o.endSpeech(in, now, true)
	}
	o.changed()
}

func signal(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

// end drops what the input holds and ends each of its playbacks with err.
// The caller holds the Output's mu.
func (in *outputInput) end(err error) {
	for p := range in.live {
		p.err = err
	}
	clear(in.live)
	in.head, in.n, in.flush = 0, 0, 0
	clear(in.segments)
	in.segments = in.segments[:0]
	in.last = nil
	in.rs.reset()
}

// idle reports whether the input has nothing left to play.
func (in *outputInput) idle() bool {
	return in.n == 0 && in.flush == 0
}

// setRate makes the input hold audio at rate. The caller has checked that
// it is idle.
func (in *outputInput) setRate(rate int) {
	in.rate = rate
	in.ring = make([]int16, max(1, int(int64(rate)*int64(InputQueue)/int64(time.Second))))
	in.head = 0
}

// push queues frames of pcm, interleaved with channels, for p.
func (in *outputInput) push(p *Playback, pcm []byte, channels, frames int) {
	for f := range frames {
		var sum int32
		for c := range channels {
			i := 2 * (f*channels + c)
			sum += int32(int16(uint16(pcm[i]) | uint16(pcm[i+1])<<8))
		}
		in.ring[(in.head+in.n)%len(in.ring)] = int16(sum / int32(channels))
		in.n++
	}
	if k := len(in.segments); k > 0 && in.segments[k-1].pb == p {
		in.segments[k-1].n += frames
	} else {
		in.segments = append(in.segments, segment{pb: p, n: frames})
	}
	p.pending += frames
}

// pull takes the next queued sample for the stream, counted as played for
// its playback, or silence to flush the resampler once none is queued.
func (in *outputInput) pull() float32 {
	if in.n == 0 {
		in.flush = max(0, in.flush-1)
		return 0
	}
	s := in.ring[in.head]
	in.head = (in.head + 1) % len(in.ring)
	in.n--
	in.flush = in.rs.lag()
	seg := &in.segments[0]
	seg.pb.pending--
	seg.pb.played.add(in.rate, 1)
	seg.pb.seq = in.seq
	in.spoken.add(in.rate, 1)
	in.last = seg.pb
	if seg.n--; seg.n == 0 {
		in.segments = in.segments[1:]
	}
	return float32(s)
}

// render fills the input's part of the next frame at the stream's rate.
func (in *outputInput) render(rate int) {
	if in.rs.from != in.rate || in.rs.to != rate {
		in.rs.configure(in.rate, rate)
	}
	if in.rs.bypass() {
		for i := range in.contrib {
			in.contrib[i] = in.pull()
		}
		return
	}
	for i := range in.contrib {
		in.contrib[i] = in.rs.output()
		for range in.rs.step() {
			in.rs.push(in.pull())
		}
	}
	if in.idle() {
		in.rs.reset()
	}
}

// run is the Output's mixer: it waits for audio, then plays a stream until
// the stream has been idle for IdleClose, and again.
func (o *Output) run() {
	defer close(o.done)
	for {
		o.mu.Lock()
		for !o.closed && !o.queued() {
			o.mu.Unlock()
			<-o.wake
			o.mu.Lock()
		}
		closed := o.closed
		o.closeNow, o.dropNow = closed, false
		o.mu.Unlock()
		if closed {
			return
		}
		o.play()
	}
}

// queued reports whether any input holds audio. The caller holds mu.
func (o *Output) queued() bool {
	for _, in := range o.order {
		if in.n > 0 {
			return true
		}
	}
	return false
}

// play opens a stream and mixes into it until it has been idle for
// IdleClose, or fails, or a teardown or Close asks it to close.
func (o *Output) play() {
	// Where playback goes through PortAudio (not Linux), PortAudio must not be
	// torn down while the stream is open: hold the output guard from before it
	// opens until after it closes (deferred first, so it is released last). A
	// teardown closes the stream and waits for it. An ALSA stream does not
	// depend on PortAudio, so a re-initialization leaves it playing.
	if playbackUsesPortAudio {
		outputs.begin(o)
		defer outputs.end(o)
	}
	var stream PlaybackStream
	// done is set once the stream is no longer in use: a panic in its close
	// after that touches no playback, which may be one begun since.
	done := false
	defer func() {
		if r := recover(); r != nil {
			slog.Error("speaker output panic recovered", "panic", r)
			if !done {
				o.mu.Lock()
				o.endAll(fmt.Errorf("speaker output: %v", r))
				o.mu.Unlock()
			}
		}
	}()
	// The output hooks bracket the stream, from before it opens until after
	// it closes, on every path out of play.
	o.cfg.Opening()
	defer o.cfg.Closing(func() {
		if stream != nil {
			_ = stream.Close()
		}
	})
	o.mu.Lock()
	closing := o.closeNow
	o.mu.Unlock()
	if closing {
		return
	}
	var rate int
	var err error
	stream, rate, err = o.cfg.Open(o.cfg.Device)
	if err == nil && rate <= 0 {
		err = fmt.Errorf("the stream reports %d Hz", rate)
	}
	if err != nil {
		o.mu.Lock()
		o.endAll(fmt.Errorf("open speaker stream: %w", err))
		o.mu.Unlock()
		done = true
		return
	}
	err = o.mix(stream, rate)
	done = true
	if err != nil {
		o.mu.Lock()
		o.endAll(err)
		o.mu.Unlock()
	}
}

// mix writes frames of the mix to stream, at rate, until the inputs have been
// idle for IdleClose or it is asked to close.
func (o *Output) mix(stream PlaybackStream, rate int) error {
	n := rate * int(outputFrame/time.Millisecond) / 1000
	frame := make([]int16, n)
	sum := make([]float32, n)
	hist := rate * int(outputHistory/time.Millisecond) / 1000
	redo := make([]int16, hist)
	var idleSince time.Time
	// The stream time written since a write last waited, at anchor.
	anchor, ahead := time.Now(), time.Duration(0)
	for {
		o.mu.Lock()
		if o.closeNow {
			o.endSpeeches(time.Now(), false)
			o.mu.Unlock()
			return nil
		}
		if o.dropNow {
			o.dropNow = false
			o.mu.Unlock()
			dropped, err := stream.Drop()
			if err != nil {
				return fmt.Errorf("drop speaker stream: %w", err)
			}
			o.mu.Lock()
			dropped = min(max(dropped, 0), hist)
			o.remix(redo[:dropped], sum, rate)
			o.mu.Unlock()
			for off := 0; off < dropped; off += n {
				if err := stream.Write(redo[off:min(off+n, dropped)]); err != nil {
					return fmt.Errorf("write speaker stream: %w", err)
				}
			}
			anchor, ahead = time.Now(), 0
			continue
		}
		now := time.Now()
		active := o.mixFrame(frame, sum, rate, hist, now)
		switch {
		case active:
			idleSince = time.Time{}
		case idleSince.IsZero():
			idleSince = now
		case now.Sub(idleSince) >= o.cfg.IdleClose:
			o.endSpeeches(now, false)
			o.mu.Unlock()
			return nil
		}
		seq := o.mixed
		o.mu.Unlock()

		began := time.Now()
		if err := stream.Write(frame); err != nil {
			return fmt.Errorf("write speaker stream: %w", err)
		}
		o.mu.Lock()
		o.written = seq
		o.changed()
		o.mu.Unlock()
		// Keep to real time on a stream that never makes a write wait.
		if time.Since(began) >= outputFrame/4 {
			anchor, ahead = time.Now(), 0
		} else if ahead += outputFrame; ahead > outputAhead {
			if wait := ahead - outputAhead - time.Since(anchor); wait > 0 {
				time.Sleep(wait)
			}
		}
	}
}

// mixFrame mixes the inputs' next frame into frame, keeps each one's part in
// its history, and reports whether any input had audio. The caller holds mu.
func (o *Output) mixFrame(frame []int16, sum []float32, rate, hist int, now time.Time) (active bool) {
	clear(sum)
	o.mixed++
	for _, in := range o.order {
		if len(in.contrib) != len(frame) || len(in.history) != hist {
			in.contrib, in.history, in.histPos = make([]float32, len(frame)), make([]float32, hist), 0
		}
		if in.idle() {
			clear(in.contrib)
			if in.speaking {
				if in.quietAt.IsZero() {
					in.quietAt = now
				} else if now.Sub(in.quietAt) >= speechEndHold {
					o.endSpeech(in, now, false)
				}
			}
		} else {
			queued := in.n > 0
			in.seq = o.mixed
			in.render(rate)
			for i, c := range in.contrib {
				sum[i] += c
			}
			if queued {
				if !in.speaking {
					in.speaking = true
					o.emit(SpeechEvent{Input: in.name, Started: true, At: now})
				}
			}
			in.lastAudio, in.quietAt = now, time.Time{}
			active = true
		}
		for _, c := range in.contrib {
			in.history[in.histPos] = c
			if in.histPos++; in.histPos == hist {
				in.histPos = 0
			}
		}
	}
	for i, s := range sum {
		frame[i] = clip(s)
	}
	return active
}

// remix mixes again, into redo, the last len(redo) samples the stream took,
// without the inputs Cancel silenced: it forgets their part, takes what the
// drop kept from the room off what they played, and ends their speeches. sum
// is scratch of at least a frame. The caller holds mu.
func (o *Output) remix(redo []int16, sum []float32, rate int) {
	dropped := time.Duration(len(redo)) * time.Second / time.Duration(rate)
	now := time.Now()
	for _, in := range o.order {
		if !in.cancelled {
			continue
		}
		clear(in.history)
		in.spoken.sub(dropped)
		if in.silenced != nil {
			in.silenced.played.sub(dropped)
			in.silenced = nil
		}
		o.endSpeech(in, now, true)
	}
	for start := 0; start < len(redo); start += len(sum) {
		part := redo[start:min(start+len(sum), len(redo))]
		clear(sum)
		for _, in := range o.order {
			h := len(in.history)
			for i := range part {
				sum[i] += in.history[(in.histPos-len(redo)+start+i+2*h)%h]
			}
		}
		for i := range part {
			part[i] = clip(sum[i])
		}
	}
	o.changed()
}

// endSpeeches ends every speech under way, as the stream closes. The caller
// holds mu.
func (o *Output) endSpeeches(now time.Time, cancelled bool) {
	for _, in := range o.order {
		o.endSpeech(in, now, cancelled)
	}
}

// endSpeech reports the input's speech ended, if one was under way. The
// caller holds mu.
func (o *Output) endSpeech(in *outputInput, now time.Time, cancelled bool) {
	if !in.speaking {
		return
	}
	at := in.lastAudio
	if cancelled {
		at = now
	}
	o.emit(SpeechEvent{Input: in.name, At: at, Played: in.spoken.total(), Cancelled: cancelled})
	in.speaking, in.cancelled, in.silenced, in.spoken, in.quietAt = false, false, nil, playedTime{}, time.Time{}
}

func clip(s float32) int16 {
	switch {
	case s >= math.MaxInt16:
		return math.MaxInt16
	case s <= math.MinInt16:
		return math.MinInt16
	default:
		return int16(math.Round(float64(s)))
	}
}

// emit queues e for the listeners. The caller holds mu.
func (o *Output) emit(e SpeechEvent) {
	o.events = append(o.events, e)
	signal(o.eventWake)
}

// dispatch calls the listeners with each event, in order, until Close.
func (o *Output) dispatch() {
	var fns []func(SpeechEvent)
	for {
		<-o.eventWake
		o.mu.Lock()
		events := o.events
		o.events = nil
		fns = fns[:0]
		for _, fn := range o.listeners {
			fns = append(fns, fn)
		}
		closed := o.closed
		o.mu.Unlock()
		for _, e := range events {
			for _, fn := range fns {
				fn(e)
			}
		}
		if closed {
			return
		}
	}
}
