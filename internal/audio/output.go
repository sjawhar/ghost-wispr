package audio

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"sync"
	"time"
)

// Inputs the Output mixes. Any other name makes an input of its own. While
// the chime input has a sound to play, the voice input is ducked (duckGain).
const (
	InputVoice = "voice"
	InputChime = "chime"
)

// duckGain is the voice input's gain while a sound plays on the chime input:
// 15 dB down. Wispr's speech reaches +0.9 dBTP and its loudest sound -3.9
// dBTP, so ducked, the speech and the sound together stay 1.5 dB under full
// scale; and each sound, 4.6 to 6.4 LU softer than the loudest of the speech,
// is 8.8 LU or more louder than the speech under it (TestSoundsOverSpeech).
// The sound itself keeps its level.
const duckGain float32 = 0.17782794 // 10^(-15/20)

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
	// Drop takes back what the device holds and the room has not heard,
	// and reports how many of the last frames written that was, for the
	// Output to write again without a cancelled input. A stream that cannot
	// tell takes back nothing and reports 0.
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
	// opened or not, and must call closeStream, which closes it and returns
	// the stream's close error (nil when none opened). The Jabra mute button
	// uses them to keep the Jabra's light from changing while a stream is open
	// on the Jabra. With no Closing, a close that fails is logged.
	Opening func()
	Closing func(closeStream func() error)
	// IdleClose is how long the stream stays open with nothing queued; 0 is
	// OutputIdleClose.
	IdleClose time.Duration
	// Open opens a stream on the device and reports its rate; nil opens the
	// speaker device (openSpeakerStream). Tests set it.
	Open func(device string) (stream PlaybackStream, rate int, err error)
	// Underrun, when set, runs on the mixer's goroutine after each write in
	// which the stream found the device run dry, as the speaker trace saves
	// its capture then (SpeakerTrace.Underrun). It must not block.
	Underrun func()
}

// SpeechEvent reports that an input's audio started or ended on the stream,
// for barge-in. A speech ends once its input has had nothing to play for
// speechEndHold, or at once on Cancel.
type SpeechEvent struct {
	Input   string
	Started bool // false: the speech ended
	// At is when the speech's first audio went to the stream, or its last,
	// on the system clock: the room hears it later, by as much as the device
	// holds (up to alsaBuffer on Linux).
	At time.Time
	// Played, on an end, is how much of the input's audio the stream took in
	// the speech, less what a Cancel took back from the device.
	Played time.Duration
	// Cancelled is true when a Cancel cut the speech: it took back audio the
	// room had not heard, or dropped audio still queued.
	Cancelled bool
}

// Output plays audio on the speaker device through one playback stream that
// mixes named inputs, such as the voice and the chime. The stream opens when
// the first frame is queued on any input, and closes once IdleClose has
// passed with nothing queued on any; Opening and Closing bracket it. Each
// input takes PCM16 at the rate its writer declares and resamples it to the
// stream's, holds at most InputQueue, and reports its speech starting and
// ending. While the chime input has a sound to play, the voice input plays
// 15 dB down (duckGain), so that the two never clip together; its gain moves
// over one frame, and down before the sound starts. Cancel silences an input
// before the next frame: it takes back what the device holds
// (PlaybackStream.Drop) and plays the other inputs' part of it again.
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
	// dropNow asks the mixer to take back what the device holds and play it
	// again without the inputs Cancel silenced.
	dropNow bool
	// waiting holds the wake channel of each Write and Drain that waits for
	// the stream to take audio or a playback to end.
	waiting []chan struct{}
	// mixed counts the frames mixed, and written the last of them the
	// stream has taken: a Drain waits for its playback's last frame.
	mixed, written uint64
	// pos is how many samples the open stream has been given, the position
	// of the next frame.
	pos int64
	// ducked is how far the voice input was ducked at the end of the last
	// frame mixed: 0 at its full gain, 1 at duckGain; voicePlayed is whether
	// it had audio in that frame.
	ducked      float32
	voicePlayed bool
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
	gen  *playGen // the end every playback begun since the last one shares

	// ring holds the queued audio, mono at rate, from head on; segments says
	// which playback each run of it belongs to, oldest first.
	ring     []int16
	head, n  int
	rate     int
	segments []segment

	rs resampler
	// flush is how many samples of silence the resampler still needs to play
	// out the whole of its response to the last queued audio (reach).
	flush int
	// last is the playback whose audio the stream took last.
	last *Playback
	seq  uint64 // the frame being mixed
	// tails are the playbacks whose samples the resampler's filter still
	// holds, each with the count of pushes (pushes) at its last: a later
	// playback keeps the filter running, and a Drain of an earlier one waits
	// for every frame its filter response reaches (render).
	tails  []tail
	pushes uint64

	speaking  bool
	cancelled bool // Cancel silenced its speech: the mixer takes it back and ends it
	cut       bool // Cancel dropped audio of the speech still queued
	lastAudio time.Time
	quietAt   time.Time // when it ran out of audio; zero while it has some
	spoken    playedTime
	contrib   []float32 // its part in the frame being mixed
	history   []float32 // its part in the stream's last outputHistory: a ring that ends at histPos
	histPos   int
	// held counts, along history, the queued samples whose output each
	// stream position holds, with the rate they were queued at (heldCount),
	// and heldBy is the playbacks they came from (owners): a Cancel takes
	// off what each playback played only its audio it took back, as long as
	// it was queued, though the speech had gaps and the resampler delays it.
	// landed and landedBy are the same for the frame being mixed and, after
	// it, the positions the resampler's delay reaches: landed[j] is the
	// stream position pos+j. unlanded is what of the queued audio the
	// resampler held when Cancel dropped it: it never reaches the stream.
	held, landed     []uint32
	heldBy, landedBy []owners
	unlanded         playedTime
}

// owners is who the queued samples a stream position holds came from: last,
// and, when an earlier playback's samples share the position, as one
// position of a downsampled input can hold the end of one playback and the
// start of the next, first and how many of them were its. A third playback
// in one position, one shorter than a resampling step, has its samples
// counted as first's.
type owners struct {
	last, first *Playback
	firstN      int
}

// tail is a playback whose last sample the resampler took at push last.
type tail struct {
	pb   *Playback
	last uint64
}

// heldCount packs n queued samples at rate, the samples whose output one
// stream position holds (outputInput.held); addHeld unpacks them.
func heldCount(n, rate int) uint32 { return uint32(n)<<24 | uint32(rate) }

// addHeld adds the samples packed in v (heldCount).
func (p *playedTime) addHeld(v uint32) {
	if v != 0 {
		p.add(int(v&(1<<24-1)), int(v>>24))
	}
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
	out  *Output
	in   *outputInput
	wake chan struct{} // signalled when a Write or Drain of it may go on
	// Guarded by out.mu:
	gen     *playGen
	pending int    // samples queued and not yet taken by the stream
	seq     uint64 // the frame that took its last sample
	played  playedTime
}

// playGen is how the playbacks begun on an input since its last end ended:
// err is nil until Cancel (ErrCancelled), a stream failure or Close
// (ErrOutputClosed) ends them all. An input that ends starts a new one, so it
// keeps no list of its playbacks.
type playGen struct{ err error }

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
		cfg.Closing = func(closeStream func() error) {
			if err := closeStream(); err != nil {
				slog.Warn("speaker stream close failed", "error", err)
			}
		}
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
		in = &outputInput{name: input, gen: &playGen{}, tails: make([]tail, 0, 4)}
		o.inputs[input] = in
		o.order = append(o.order, in)
	}
	p := &Playback{out: o, in: in, wake: make(chan struct{}, 1), gen: in.gen}
	if o.closed {
		p.gen = &playGen{err: ErrOutputClosed}
	}
	return p
}

// Write queues frame, at most MaxWriteFrame of whole PCM16 frames in format,
// whose channels it mixes down to one. It waits while the input holds
// InputQueue, or holds audio at another rate. Once the playback has ended it
// returns why: ErrCancelled, the stream's failure, or ErrOutputClosed; once
// ctx is done, ctx's error, having queued nothing.
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
		if p.gen.err != nil {
			return p.gen.err
		}
		// A writer whose context is done queues nothing: its exchange may
		// have been stopped just before this playback began, after the
		// Cancel that ended the ones before it.
		if err := ctx.Err(); err != nil {
			return err
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
		if err := o.waitRoom(ctx, p.wake); err != nil {
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

// maxRate and maxChannels bound the PCM16 a Write takes: past them the
// resampler's filter grows long enough to keep the mixer from real time.
const (
	maxRate     = 384000
	maxChannels = 8
)

// wholeFrames reports how many whole frames pcm holds in format.
func wholeFrames(pcm []byte, format AudioFormat) (int, error) {
	if format.Encoding != EncodingPCM16 || format.SampleRate <= 0 || format.SampleRate > maxRate ||
		format.Channels <= 0 || format.Channels > maxChannels {
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
		if p.gen.err != nil {
			return p.gen.err
		}
		if p.pending == 0 && (in.last != p || in.idle()) && o.written >= p.seq {
			return nil
		}
		if err := o.waitRoom(ctx, p.wake); err != nil {
			return err
		}
	}
}

// Played reports how much of the playback's audio the stream has taken, less
// what a Cancel took back from the device before the room heard it.
func (p *Playback) Played() time.Duration {
	p.out.mu.Lock()
	defer p.out.mu.Unlock()
	return p.played.total()
}

// waitRoom waits, with mu released, until the stream takes audio or a
// playback ends, signalled on wake, the waiting playback's. The caller holds
// mu.
func (o *Output) waitRoom(ctx context.Context, wake chan struct{}) error {
	o.waiting = append(o.waiting, wake)
	o.mu.Unlock()
	defer o.mu.Lock()
	select {
	case <-wake:
		return nil
	case <-ctx.Done():
		o.mu.Lock()
		if i := slices.Index(o.waiting, wake); i >= 0 {
			o.waiting = slices.Delete(o.waiting, i, i+1)
		}
		o.mu.Unlock()
		return ctx.Err()
	}
}

// changed wakes every Write and Drain that waits. The caller holds mu.
func (o *Output) changed() {
	for _, wake := range o.waiting {
		signal(wake)
	}
	clear(o.waiting)
	o.waiting = o.waiting[:0]
}

// Cancel silences input: it drops what the input holds, ends each of its
// playbacks with ErrCancelled, and has the mixer take back what the device
// holds and play the other inputs' part of it again, before its next frame
// (PlaybackStream.Drop). A playback begun after Cancel plays.
func (o *Output) Cancel(input string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	in := o.inputs[input]
	if in == nil {
		return
	}
	cut := !in.idle()
	in.end(ErrCancelled)
	if in.speaking {
		// A second Cancel before the mixer takes the first in finds nothing
		// queued: it keeps the first one's cut.
		in.cancelled, in.cut = true, in.cut || cut
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

// end drops what the input holds and ends each of its playbacks with err: the
// generation they share ends, and a new one begins. The audio the resampler
// held, which will not reach the stream, comes off what its playbacks played,
// and is kept in unlanded for a Cancel's take-back of the speech. The caller
// holds the Output's mu.
func (in *outputInput) end(err error) {
	in.gen.err = err
	in.gen = &playGen{}
	in.head, in.n, in.flush = 0, 0, 0
	clear(in.segments)
	in.segments = in.segments[:0]
	in.last = nil
	in.rs.reset()
	clear(in.tails)
	in.tails = in.tails[:0]
	takeBack(in.landed, in.landedBy, &in.unlanded)
	clear(in.landed)
	clear(in.landedBy)
}

// takeBack adds the queued samples counted in held to lost, and takes each
// playback's share of them off what it played (heldBy). The caller holds the
// Output's mu.
func takeBack(held []uint32, heldBy []owners, lost *playedTime) {
	type share struct {
		pb *Playback
		t  playedTime
	}
	var shares []share
	charge := func(pb *Playback, rate, n int) {
		if pb == nil || n <= 0 {
			return
		}
		k := slices.IndexFunc(shares, func(s share) bool { return s.pb == pb })
		if k < 0 {
			shares, k = append(shares, share{pb: pb}), len(shares)
		}
		shares[k].t.add(rate, n)
	}
	for i, v := range held {
		if v == 0 {
			continue
		}
		lost.addHeld(v)
		rate, n, o := int(v&(1<<24-1)), int(v>>24), heldBy[i]
		first := 0
		if o.first != nil {
			first = min(o.firstN, n)
			charge(o.first, rate, first)
		}
		charge(o.last, rate, n-first)
	}
	for _, s := range shares {
		s.pb.played.sub(s.t.total())
	}
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
// its playback, which it returns; or silence, and nil, to flush the
// resampler once none is queued.
func (in *outputInput) pull() (float32, *Playback) {
	if in.n == 0 {
		in.flush = max(0, in.flush-1)
		return 0, nil
	}
	s := in.ring[in.head]
	in.head = (in.head + 1) % len(in.ring)
	in.n--
	in.flush = in.rs.reach()
	pb := in.segments[0].pb
	pb.pending--
	pb.played.add(in.rate, 1)
	in.spoken.add(in.rate, 1)
	in.last = pb
	if in.segments[0].n--; in.segments[0].n == 0 {
		// Shift the rest down, so that the array keeps no finished playback
		// and appends reuse it.
		k := copy(in.segments, in.segments[1:])
		in.segments[k] = segment{}
		in.segments = in.segments[:k]
	}
	return float32(s), pb
}

// render fills the input's part of the next frame at the stream's rate. It
// counts each sample it takes at the stream position its output reaches,
// after the resampler's delay (landed), and marks the frame for the
// sample's playback, whose Drain waits for it. A playback whose samples the
// resampler's filter still holds when the frame is done, as when a later
// playback keeps it running, has audio in the next frame too, which this
// input renders, as it flushes the filter until they leave (reach): it is
// marked for that frame now, before the stream takes this one and Drain can
// wake (tails).
func (in *outputInput) render(rate int) {
	if in.rs.from != in.rate || in.rs.to != rate {
		in.rs.configure(in.rate, rate)
	}
	if in.rs.bypass() {
		for i := range in.contrib {
			s, pb := in.pull()
			if pb != nil {
				in.land(i, pb)
				pb.seq = max(pb.seq, in.seq)
			}
			in.contrib[i] = s
		}
		return
	}
	lag := in.rs.lag() * rate / in.rate // output samples a pushed sample takes to reach the output
	if need := len(in.contrib) + lag + 1; len(in.landed) < need {
		grown, grownBy := make([]uint32, need), make([]owners, need)
		copy(grown, in.landed)
		copy(grownBy, in.landedBy)
		in.landed, in.landedBy = grown, grownBy
	}
	for i := range in.contrib {
		in.contrib[i] = in.rs.output()
		for range in.rs.step() {
			s, pb := in.pull()
			in.rs.push(s)
			in.pushes++
			if pb == nil {
				continue
			}
			in.land(i+lag, pb)
			pb.seq = max(pb.seq, in.seq)
			if k := len(in.tails); k > 0 && in.tails[k-1].pb == pb {
				in.tails[k-1].last = in.pushes
			} else {
				in.tails = append(in.tails, tail{pb: pb, last: in.pushes})
			}
		}
	}
	// The next frame's first output reads the last taps samples pushed: a
	// playback with one of them has audio there. One whose last sample has
	// left the filter has none.
	taps, kept := uint64(in.rs.taps), in.tails[:0]
	for _, t := range in.tails {
		if in.pushes-t.last < taps {
			t.pb.seq = max(t.pb.seq, in.seq+1)
			kept = append(kept, t)
		}
	}
	clear(in.tails[len(kept):])
	in.tails = kept
	if in.idle() {
		in.rs.reset()
		clear(in.tails)
		in.tails = in.tails[:0]
	}
}

// land counts one queued sample of pb, at the input's rate, at landed[j]. A
// position whose samples so far came from another playback keeps that one as
// its first owner (owners).
func (in *outputInput) land(j int, pb *Playback) {
	n := int(in.landed[j] >> 24)
	if o := &in.landedBy[j]; o.last != pb {
		if o.last != nil {
			if o.first == nil {
				o.first = o.last
			}
			o.firstN = n
		}
		o.last = pb
	}
	in.landed[j] = heldCount(n+1, in.rate)
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
	// A panic in a hook or in the stream's close touches no playback: one
	// begun once the stream closed may be playing on the next.
	defer func() {
		if r := recover(); r != nil {
			slog.Error("speaker output panic recovered", "panic", r)
		}
	}()
	var stream PlaybackStream
	// The output hooks bracket the stream, from before it opens until after
	// it closes, on every path out of play.
	o.cfg.Opening()
	defer o.cfg.Closing(func() error {
		if stream == nil {
			return nil
		}
		return stream.Close()
	})
	// A panic while the stream is in use ends every playback before the
	// stream closes (deferred last, so it runs first).
	defer func() {
		if r := recover(); r != nil {
			slog.Error("speaker output panic recovered", "panic", r)
			o.fail(fmt.Errorf("speaker output: %v", r))
		}
	}()
	if o.closing() {
		return
	}
	var rate int
	var err error
	stream, rate, err = o.cfg.Open(o.cfg.Device)
	if err == nil && rate <= 0 {
		err = fmt.Errorf("the stream reports %d Hz", rate)
	}
	if err != nil {
		o.fail(fmt.Errorf("open speaker stream: %w", err))
		return
	}
	if err := o.mix(stream, rate); err != nil {
		o.fail(err)
	}
}

// closing reports whether a teardown or Close asks the mixer to close.
func (o *Output) closing() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.closeNow
}

// fail ends every playback with err, as when the stream fails.
func (o *Output) fail(err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.endAll(err)
}

// mixStep is what the mixer does next.
type mixStep int

const (
	stepWrite mixStep = iota // write the frame just mixed
	stepIdle                 // the inputs have been idle for IdleClose: close once the device holds only silence
	stepDrop                 // take back what the device holds, for a Cancel
	stepClose                // close the stream
)

// mix writes frames of the mix to stream, at rate, until the inputs have been
// idle for IdleClose or it is asked to close. Each step that touches the
// inputs runs under mu in a method of its own that unlocks on every path, a
// panic's included.
func (o *Output) mix(stream PlaybackStream, rate int) error {
	n := rate * int(outputFrame/time.Millisecond) / 1000
	frame := make([]int16, n)
	sum := make([]float32, n)
	hist := rate * int(outputHistory/time.Millisecond) / 1000
	if b, ok := stream.(interface{ bufferFrames() int }); ok {
		// What a granted buffer holds, what may be in flight to a USB
		// speaker past it, which a drop also cuts (bounded here by the
		// buffer again), and a frame.
		hist = max(hist, 2*b.bufferFrames()+n)
	}
	// again, when the stream has it, reports how many of the last frames it
	// was given it discarded by itself, as in a suspend it could not resume
	// from: they play again, as what a Drop takes back does.
	again := func() int { return 0 }
	if a, ok := stream.(interface{ takeAgain() int }); ok {
		again = a.takeAgain
	}
	redo := make([]int16, hist)
	o.startStream()
	var idleSince time.Time
	p := pacer{anchor: time.Now()}
	// quiet is how many frames of silence the stream has been given since
	// the last audio: once it holds no more than that, it plays only silence.
	quiet := 0
	// counter is the stream's count of underruns, when it keeps one, and
	// underruns the count at the last write: a write that raises it ran the
	// device dry (OutputConfig.Underrun).
	counter, _ := stream.(interface{ underruns() int })
	underruns := 0
	for {
		step, seq, active := o.next(frame, sum, rate, hist, &idleSince)
		switch step {
		case stepClose:
			return nil
		case stepIdle:
			if q, ok := stream.(interface{ queuedFrames() int }); !ok || quiet >= q.queuedFrames() {
				o.closeIdle()
				return nil
			}
		case stepDrop:
			got, err := stream.Drop()
			if err != nil {
				return fmt.Errorf("take back speaker stream: %w", err)
			}
			if err := o.replay(stream, redo, sum, n, max(got, 0), again); err != nil {
				return err
			}
			continue
		}
		began := time.Now()
		err := writeFrame(stream, frame)
		if err != nil {
			return fmt.Errorf("write speaker stream: %w", err)
		}
		if lost := again(); lost > 0 {
			// The frame and some before it the device discarded: they play
			// again before the frame counts as taken.
			if err := o.replay(stream, redo, sum, n, lost, again); err != nil {
				return err
			}
		}
		if counter != nil && o.cfg.Underrun != nil {
			if n := counter.underruns(); n > underruns {
				underruns = n
				o.cfg.Underrun()
			}
		}
		o.wrote(seq)
		p.wrote(time.Since(began), time.Now())
		if active {
			quiet = 0
		} else {
			quiet += n
		}
	}
}

// replay writes the stream again the last back frames it was given, which a
// Drop took back or the stream discarded by itself (again), mixed again
// without the inputs a Cancel silenced: the stream time written is as
// before, so the pacer is not touched. A Cancel during it takes back the
// device's audio again, and a discard during it is written again too, with
// what of the replay was left unwritten.
func (o *Output) replay(stream PlaybackStream, redo []int16, sum []float32, n, back int, again func() int) error {
	for {
		if back > len(redo) {
			slog.Warn("speaker stream took back more than the Output keeps; the rest of the other inputs is lost", "frames", back, "kept", len(redo))
			back = len(redo)
		}
		o.retake(redo[:back], sum)
		off, drop, lost := 0, false, 0
		for off < back && !drop && lost == 0 {
			end := min(off+n, back)
			if err := stream.Write(redo[off:end]); err != nil {
				return fmt.Errorf("write speaker stream: %w", err)
			}
			off, lost = end, again()
			drop = lost == 0 && end < back && o.takeDrop()
		}
		unwritten := back - off
		switch {
		case lost > 0:
			back = lost + unwritten
		case drop:
			got, err := stream.Drop()
			if err != nil {
				return fmt.Errorf("take back speaker stream: %w", err)
			}
			back = max(got, 0) + unwritten
		default:
			return nil
		}
	}
}

// pacer keeps the mixer to real time on a stream that never makes a write
// wait, as ALSA's null PCM: it holds a write back once the stream time
// written since anchor runs more than outputAhead ahead of the time passed.
// A stream that makes writes wait keeps time itself: after a run of such
// writes the pacer takes its clock as the anchor, so that a device whose
// clock runs a little fast is never held back. One slow write, as when the
// process waited for a processor, does not count as the stream waiting.
type pacer struct {
	anchor  time.Time
	written time.Duration
	waited  int // writes in a row the stream made wait
}

// pacerWaits is how many writes in a row a stream must make wait, by half a
// frame or more, before the pacer takes it to keep time.
const pacerWaits = 4

func (p *pacer) wrote(took time.Duration, now time.Time) {
	p.written += outputFrame
	if took >= outputFrame/2 {
		p.waited++
	} else {
		p.waited = 0
	}
	if p.waited >= pacerWaits {
		p.anchor, p.written = now, 0
		return
	}
	if ahead := p.written - now.Sub(p.anchor); ahead > outputAhead {
		pacerSleep(ahead - outputAhead)
	}
}

// startStream starts the stream positions of a new stream.
func (o *Output) startStream() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.pos = 0
}

// next decides the mixer's next step: close, take back for a Cancel, or
// write the frame it mixes into frame, whose number it returns, with whether
// any input had audio in it. It returns stepIdle, with a frame of silence to
// write, once the inputs have been idle for IdleClose.
func (o *Output) next(frame []int16, sum []float32, rate, hist int, idleSince *time.Time) (step mixStep, seq uint64, active bool) {
	o.lockForMix()
	defer o.mu.Unlock()
	now := time.Now()
	if o.closeNow {
		o.endSpeeches(now, false)
		return stepClose, 0, false
	}
	if o.dropNow {
		o.dropNow = false
		return stepDrop, 0, false
	}
	active = o.mixFrame(frame, sum, rate, hist, now)
	switch {
	case active:
		*idleSince = time.Time{}
	case idleSince.IsZero():
		*idleSince = now
	case now.Sub(*idleSince) >= o.cfg.IdleClose:
		return stepIdle, o.mixed, false
	}
	return stepWrite, o.mixed, active
}

// closeIdle ends every speech as the stream closes for being idle.
func (o *Output) closeIdle() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.endSpeeches(time.Now(), false)
}

// takeDrop reports whether a Cancel asked, since the mixer last looked, to
// take back the device's audio, and takes the request.
func (o *Output) takeDrop() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	drop := o.dropNow
	o.dropNow = false
	return drop
}

// wrote records that the stream has taken frame seq, and wakes every Write
// and Drain that waits.
func (o *Output) wrote(seq uint64) {
	o.lockForMix()
	defer o.mu.Unlock()
	o.written = seq
	o.changed()
}

// mixFrame mixes the inputs' next frame into frame, keeps each one's part in
// its history, and reports whether any input had audio. The caller holds mu.
func (o *Output) mixFrame(frame []int16, sum []float32, rate, hist int, now time.Time) (active bool) {
	clear(sum)
	o.mixed++
	voice, chime := o.inputs[InputVoice], o.inputs[InputChime]
	from, to, hold := o.duck(voice, chime)
	for _, in := range o.order {
		if len(in.contrib) != len(frame) || len(in.history) != hist {
			in.contrib, in.history, in.held, in.histPos = make([]float32, len(frame)), make([]float32, hist), make([]uint32, hist), 0
			in.heldBy = make([]owners, hist)
			in.landed, in.landedBy = make([]uint32, 2*len(frame)), make([]owners, 2*len(frame))
		}
		switch {
		case in == chime && hold:
			// The voice goes down over this frame; the sound starts on the next.
			clear(in.contrib)
		case in.idle():
			clear(in.contrib)
			if in.speaking {
				if in.quietAt.IsZero() {
					in.quietAt = now
				} else if now.Sub(in.quietAt) >= speechEndHold {
					o.endSpeech(in, now, false)
				}
			}
		default:
			queued := in.n > 0
			in.seq = o.mixed
			in.render(rate)
			if in == voice {
				duckVoice(in.contrib, from, to)
			}
			for i, c := range in.contrib {
				sum[i] += c
			}
			if queued && !in.speaking {
				in.speaking, in.unlanded = true, playedTime{}
				o.emit(SpeechEvent{Input: in.name, Started: true, At: now})
			}
			in.lastAudio, in.quietAt = now, time.Time{}
			active = true
		}
		for i, c := range in.contrib {
			in.history[in.histPos], in.held[in.histPos], in.heldBy[in.histPos] = c, in.landed[i], in.landedBy[i]
			if in.histPos++; in.histPos == hist {
				in.histPos = 0
			}
		}
		// The counts past the frame, the resampler's delay, move to its start.
		k := copy(in.landed, in.landed[len(frame):])
		clear(in.landed[k:])
		copy(in.landedBy, in.landedBy[len(frame):])
		clear(in.landedBy[k:])
	}
	for i, s := range sum {
		frame[i] = clip(s)
	}
	o.pos += int64(len(frame))
	return active
}

// duck decides how the voice input is ducked over the next frame: from and
// to are how far down it is at the frame's start and end (0 at its full gain,
// 1 at duckGain), and hold keeps the chime input's sound back for the frame,
// so that the voice is down before the sound starts. A voice that played
// nothing in the last frame takes its gain at once: no one heard it change.
// The caller holds mu.
func (o *Output) duck(voice, chime *outputInput) (from, to float32, hold bool) {
	if chime != nil && !chime.idle() {
		to = 1
	}
	from = o.ducked
	playing := voice != nil && !voice.idle()
	if !playing || !o.voicePlayed {
		from = to
	}
	o.ducked, o.voicePlayed = to, playing
	return from, to, from < to
}

// duckVoice scales the voice's part of a frame from how far down it is at
// the frame's start to how far at its end, linearly over the frame, so that
// a change of gain does not click.
func duckVoice(contrib []float32, from, to float32) {
	if from == 0 && to == 0 {
		return
	}
	n := float32(len(contrib))
	for i := range contrib {
		down := from + (to-from)*float32(i+1)/n
		contrib[i] *= 1 - down*(1-duckGain)
	}
}

// retake mixes again, into redo, the last len(redo) samples the stream took
// back, without the inputs Cancel silenced: it forgets their part, takes the
// part of their audio the room will not hear off what they played, and ends
// their speeches. sum is scratch of at least a frame.
func (o *Output) retake(redo []int16, sum []float32) {
	o.mu.Lock()
	defer o.mu.Unlock()
	back := len(redo)
	now := time.Now()
	for _, in := range o.order {
		if !in.cancelled {
			continue
		}
		// The stream took back [pos-back, pos); of the speech's audio, the
		// room will not hear the queued samples whose output lies there, nor
		// those the resampler held when Cancel dropped them (unlanded): as
		// long as they were queued, at their rate, and only them, since a
		// gap in the speech was silence.
		lost, h := in.unlanded, len(in.held)
		if n := min(back, h); n > 0 {
			from := (in.histPos - n + h) % h
			if from+n <= h {
				takeBack(in.held[from:from+n], in.heldBy[from:from+n], &lost)
			} else {
				takeBack(in.held[from:], in.heldBy[from:], &lost)
				takeBack(in.held[:from+n-h], in.heldBy[:from+n-h], &lost)
			}
		}
		in.unlanded = playedTime{}
		clear(in.history)
		clear(in.held)
		clear(in.heldBy)
		unheard := lost.total()
		in.spoken.sub(unheard)
		o.endSpeech(in, now, in.cut || unheard > 0)
	}
	for start := 0; start < len(redo); start += len(sum) {
		part := redo[start:min(start+len(sum), len(redo))]
		clear(sum)
		for _, in := range o.order {
			h := len(in.history)
			if h == 0 {
				continue // begun since the mixer last ran: it played nothing here
			}
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
	in.speaking, in.cancelled, in.cut, in.spoken, in.quietAt = false, false, false, playedTime{}, time.Time{}
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
