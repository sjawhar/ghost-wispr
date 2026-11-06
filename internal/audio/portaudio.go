package audio

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/gordonklaus/portaudio"
)

var paMu sync.Mutex

var (
	paTerminate  = portaudio.Terminate
	paInitialize = portaudio.Initialize
)

// outputStopWait is how long a teardown of PortAudio waits for speaker
// playbacks to stop. A playback stops at its next 250 ms buffer.
var outputStopWait = 5 * time.Second

// errOutputStillOpen reports a speaker output stream that did not close in
// time; PortAudio is left as it is rather than torn down under it.
var errOutputStillOpen = errors.New("a speaker output stream is still open")

// outputs keeps PortAudio from being torn down under a speaker's output
// stream, which crashes inside PortAudio. A playback holds it from before its
// stream opens until after the stream closes.
var outputs = newOutputGuard()

type outputGuard struct {
	mu          sync.Mutex
	changed     *sync.Cond // signalled (with mu) when a playback ends or a teardown finishes
	open        int        // playbacks holding an output stream
	tearingDown bool       // PortAudio is being torn down: no stream may open
	playing     map[*Speaker]struct{}
}

func newOutputGuard() *outputGuard {
	g := &outputGuard{playing: map[*Speaker]struct{}{}}
	g.changed = sync.NewCond(&g.mu)
	return g
}

// begin waits while PortAudio is being torn down, then counts s's playback as
// holding an output stream until end.
func (g *outputGuard) begin(s *Speaker) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for g.tearingDown {
		g.changed.Wait()
	}
	g.open++
	g.playing[s] = struct{}{}
}

func (g *outputGuard) end(s *Speaker) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.open--
	delete(g.playing, s)
	g.changed.Broadcast()
}

// stopAll stops every playback and waits, up to wait, until their streams have
// closed; new playbacks wait until resume. It reports false, and lets
// playbacks go on, if a stream is still open after wait.
func (g *outputGuard) stopAll(wait time.Duration) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.tearingDown = true
	for s := range g.playing {
		_ = s.Stop()
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
