package audio

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/gordonklaus/portaudio"
)

var paMu sync.Mutex

// PortAudio's terminate and initialize; tests replace them. A real
// initialization connects to the sound server ALSA's default PCM names and
// lists its devices, so a test binary refuses it (realDevice).
var (
	paTerminate  = portaudio.Terminate
	paInitialize = func() error {
		if err := realDevice("PortAudio's initialization, which lists the sound server's devices"); err != nil {
			return err
		}
		return portaudio.Initialize()
	}
)

// UsePlugCapture makes every later PortAudio initialization list each ALSA
// card's devices through ALSA's plug layer, as "plughw:N,M" rather than the
// raw "hw:N,M": Pa_Initialize reads PA_ALSA_PLUGHW (PortAudio 19.6,
// pa_linux_alsa.c:1250-1256). With device set, the mic_device setting, the
// microphone then opens the named card at the rate the start asks for, 16000
// Hz, which ALSA converts from the rate the card takes: the Jabra Speak2 75
// captures only at 32000 Hz, and its raw device refused 16000. Call it before
// the first initialization. With device empty it changes nothing, and the
// default input opens as before.
func UsePlugCapture(device string) error {
	if device == "" {
		return nil
	}
	return os.Setenv("PA_ALSA_PLUGHW", "1")
}

// outputStopWait is how long a teardown of PortAudio waits for the speaker's
// stream to close. The Output closes it before its next frame (outputFrame).
var outputStopWait = 5 * time.Second

// errOutputStillOpen reports a speaker output stream that did not close in
// time; PortAudio is left as it is rather than torn down under it.
var errOutputStillOpen = errors.New("a speaker output stream is still open")

// outputs keeps PortAudio from being torn down under the speaker's output
// stream, which crashes inside PortAudio. An Output holds it from before its
// stream opens until after the stream closes.
var outputs = newOutputGuard()

type outputGuard struct {
	mu          sync.Mutex
	changed     *sync.Cond // signalled (with mu) when a stream closes or a teardown finishes
	open        int        // Outputs holding an output stream
	tearingDown bool       // PortAudio is being torn down: no stream may open
	playing     map[*Output]struct{}
}

func newOutputGuard() *outputGuard {
	g := &outputGuard{playing: map[*Output]struct{}{}}
	g.changed = sync.NewCond(&g.mu)
	return g
}

// begin waits while PortAudio is being torn down, then counts o as holding an
// output stream until end.
func (g *outputGuard) begin(o *Output) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for g.tearingDown {
		g.changed.Wait()
	}
	g.open++
	g.playing[o] = struct{}{}
}

func (g *outputGuard) end(o *Output) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.open--
	delete(g.playing, o)
	g.changed.Broadcast()
}

// stopAll has every Output close its stream and waits, up to wait, until the
// streams have closed; new streams wait until resume. It reports false, and
// lets the Outputs go on, if a stream is still open after wait.
func (g *outputGuard) stopAll(wait time.Duration) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.tearingDown = true
	for o := range g.playing {
		o.stopForTeardown()
	}
	expired := false
	timer := time.AfterFunc(wait, func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		expired = true
		g.changed.Broadcast()
	})
	defer timer.Stop()
	for g.open > 0 && !expired {
		g.changed.Wait()
	}
	if g.open > 0 {
		g.tearingDown = false
		g.changed.Broadcast()
		return false
	}
	return true
}

// resume lets playbacks open streams again after a teardown.
func (g *outputGuard) resume() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.tearingDown = false
	g.changed.Broadcast()
}

func InitializePortAudio() error {
	paMu.Lock()
	defer paMu.Unlock()
	return paInitialize()
}

// TerminatePortAudio stops speaker playback and terminates PortAudio once
// every output stream has closed. If one is still open after outputStopWait,
// it returns an error and leaves PortAudio to the process exit.
func TerminatePortAudio() error {
	paMu.Lock()
	defer paMu.Unlock()
	if !outputs.stopAll(outputStopWait) {
		return fmt.Errorf("terminate portaudio: %w", errOutputStillOpen)
	}
	defer outputs.resume()
	return paTerminate()
}

// ReinitPortAudio terminates and initializes PortAudio again, to pick up new
// devices, once speaker playback has stopped and every output stream has
// closed. If one is still open after outputStopWait, it returns an error and
// leaves PortAudio as it is.
func ReinitPortAudio() error {
	paMu.Lock()
	defer paMu.Unlock()
	if !outputs.stopAll(outputStopWait) {
		return fmt.Errorf("reinitialize portaudio: %w", errOutputStillOpen)
	}
	defer outputs.resume()

	termErr := paTerminate()
	initErr := paInitialize()
	if initErr != nil {
		if termErr != nil {
			return fmt.Errorf("reinitialize portaudio: terminate: %v; initialize: %w", termErr, initErr)
		}
		return fmt.Errorf("reinitialize portaudio: %w", initErr)
	}

	return nil
}
