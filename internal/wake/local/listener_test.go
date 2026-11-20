package local

import (
	"bytes"
	"context"
	"errors"
	"go/parser"
	"go/token"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testWait = 5 * time.Second

// fakeCapture is the microphone stream: each Read returns the next buffer a
// test sends on reads, or blocks until Close.
type fakeCapture struct {
	reads  chan []int16
	closed chan struct{}
	once   sync.Once
}

func newFakeCapture() *fakeCapture {
	return &fakeCapture{reads: make(chan []int16), closed: make(chan struct{})}
}

func (c *fakeCapture) Read(pcm []int16) (int, error) {
	select {
	case buf := <-c.reads:
		return copy(pcm, buf), nil
	case <-c.closed:
		return 0, errors.New("capture closed")
	case <-time.After(20 * time.Millisecond):
		return copy(pcm, make([]int16, len(pcm))), nil // a buffer of silence, as a period passes
	}
}

func (c *fakeCapture) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

func (c *fakeCapture) isClosed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

// listenerRig runs a listener on a fake capture until the test ends.
type listenerRig struct {
	detector *Detector
	spot     *fakeSpotter
	verify   *fakeVerifier
	want     atomic.Bool
	opens    chan *fakeCapture
	heard    atomic.Int32
	failures chan error
	openErr  atomic.Pointer[error]
	listener *Listener
	// atHeard, when set by a setup, runs inside the listener's resume request.
	atHeard func()
}

func startListenerRig(t *testing.T, hits []int, setup ...func(*listenerRig)) *listenerRig {
	t.Helper()
	r := &listenerRig{
		spot:     &fakeSpotter{hits: hits},
		verify:   &fakeVerifier{text: "WHISPER START RECORDING"},
		opens:    make(chan *fakeCapture, 8),
		failures: make(chan error, 8),
	}
	for _, s := range setup {
		s(r)
	}
	r.detector = NewDetectorOf(r.spot, r.verify)
	r.listener = NewListener(r.detector, ListenerConfig{
		Open: func() (Capture, error) {
			if err := r.openErr.Load(); err != nil {
				return nil, *err
			}
			if !r.want.Load() {
				return nil, ErrNotNow
			}
			c := newFakeCapture()
			r.opens <- c
			return c, nil
		},
		Want: r.want.Load,
		Heard: func() {
			r.heard.Add(1)
			if r.atHeard != nil {
				r.atHeard()
			}
			r.want.Store(false)
		},
		Failed: func(err error) { r.failures <- err },
		Poll:   time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.listener.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(testWait):
			t.Error("the listener did not stop")
		}
	})
	return r
}

func (r *listenerRig) opened(t *testing.T) *fakeCapture {
	t.Helper()
	select {
	case c := <-r.opens:
		return c
	case <-time.After(testWait):
		t.Fatal("the listener did not open its capture")
		return nil
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(testWait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestListenerOpensOnlyWhileWanted fails if the listener opens its capture
// while the unit is not listening, or keeps it open once it is not.
func TestListenerOpensOnlyWhileWanted(t *testing.T) {
	r := startListenerRig(t, nil)
	time.Sleep(20 * time.Millisecond)
	select {
	case <-r.opens:
		t.Fatal("the listener opened its capture while the unit was not listening")
	default:
	}
	r.want.Store(true)
	c := r.opened(t)
	r.want.Store(false)
	waitFor(t, "the capture to close once the unit stopped listening", c.isClosed)
}

// TestRingZeroedOnStateChange fails if audio from one stretch of listening
// stays in the ring, or in the conversion buffer, once the state changes:
// when the unit stops listening, and when Interrupt closes the stream for a
// recording microphone's open.
func TestRingZeroedOnStateChange(t *testing.T) {
	r := startListenerRig(t, nil)
	r.want.Store(true)
	c := r.opened(t)
	c.reads <- samplesOf(1, SampleRate/10)
	r.want.Store(false)
	waitFor(t, "the capture to close", c.isClosed)
	waitFor(t, "the ring to be zeroed once the unit stopped listening", func() bool {
		r.listener.mu.Lock()
		defer r.listener.mu.Unlock()
		return !r.listener.open && allZero(r.detector)
	})

	r.want.Store(true)
	c = r.opened(t)
	c.reads <- samplesOf(1, SampleRate/10)
	r.listener.Interrupt()
	waitFor(t, "the capture to close at an interrupt", c.isClosed)
	// The unit still listens, so the listener opens a new stream, which has
	// given it nothing but silence: the ring must start from zero.
	r.opened(t)
	r.listener.mu.Lock()
	zeroed := allZero(r.detector)
	r.listener.mu.Unlock()
	if !zeroed {
		t.Fatal("the stream that opened after the interrupt found the last stream's audio in the ring")
	}
}

// TestTheListenerClosesBeforeItAsksToResume fails if the listener asks for the
// resume with its capture still open: the resume's microphone must open only
// after the listener's has closed.
func TestTheListenerClosesBeforeItAsksToResume(t *testing.T) {
	closedAtHeard := make(chan bool, 1)
	var current atomic.Pointer[fakeCapture]
	r := startListenerRig(t, []int{SampleRate / 10}, func(r *listenerRig) {
		r.atHeard = func() { closedAtHeard <- current.Load().isClosed() }
	})
	r.want.Store(true)
	c := r.opened(t)
	current.Store(c)
	c.reads <- samplesOf(1, SampleRate/10)
	select {
	case closed := <-closedAtHeard:
		if !closed {
			t.Fatal("the listener asked to resume with its capture open")
		}
	case <-time.After(testWait):
		t.Fatal("the listener did not ask to resume after the phrase")
	}
}

// TestAFailedCaptureIsReported fails if a capture that will not open, or that
// fails while it reads, goes unreported: the unit must then close the
// microphone's state, since nothing listens.
func TestAFailedCaptureIsReported(t *testing.T) {
	r := startListenerRig(t, nil)
	busy := errors.New("device or resource busy")
	r.openErr.Store(&busy)
	r.want.Store(true)
	select {
	case err := <-r.failures:
		if !errors.Is(err, busy) {
			t.Fatalf("reported %v, want the open's error", err)
		}
	case <-time.After(testWait):
		t.Fatal("a capture that will not open was not reported")
	}
}

// TestListenerSendsNoEvent fails if the listener can reach anything but its
// resume request: it imports no package that sends an event, an Envoy
// message, a network request or a stored row, and its log lines hold none of
// stage 2's words, even when it hears the phrase and when stage 2 refuses it.
func TestListenerSendsNoEvent(t *testing.T) {
	for _, path := range sourceFiles(t) {
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, spec := range file.Imports {
			imported, _ := strconv.Unquote(spec.Path.Value)
			for _, banned := range []string{"net", "database/sql", "os/exec", "github.com/sjawhar/ghost-wispr/"} {
				if imported == banned || strings.HasPrefix(imported, strings.TrimSuffix(banned, "/")+"/") {
					t.Errorf("%s imports %s: the listener's only output is its resume request", filepath.Base(path), imported)
				}
			}
		}
	}

	logged := &lockedBuffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logged, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	for _, text := range []string{"WHISPER START RECORDING", "WHISPER STOP RECORDING AT TEN"} {
		r := startListenerRig(t, []int{SampleRate / 10}, func(r *listenerRig) { r.verify.text = text })
		r.want.Store(true)
		c := r.opened(t)
		c.reads <- samplesOf(1, SampleRate/10)
		waitFor(t, "stage 2 to run", func() bool {
			r.listener.mu.Lock()
			defer r.listener.mu.Unlock()
			return len(r.verify.given) > 0
		})
	}
	if !strings.Contains(logged.String(), "paused listener") {
		t.Fatalf("control: the listener logged nothing, so the check below reads no line: %q", logged.String())
	}
	for _, word := range []string{"WHISPER", "START", "STOP", "RECORDING", "TEN"} {
		if regexp.MustCompile(`(?i)\b` + word + `\b`).MatchString(logged.String()) {
			t.Fatalf("the listener logged a word stage 2 heard, %q: %s", word, logged.String())
		}
	}
}

// lockedBuffer is a log sink that two listeners may write at once.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// sourceFiles lists the package's non-test Go files.
func sourceFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") {
			files = append(files, name)
		}
	}
	if len(files) == 0 {
		t.Fatal("UNKNOWN: no source files read")
	}
	return files
}
