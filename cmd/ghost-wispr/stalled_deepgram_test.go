package main

// The #65 review's stalled-peer probe (B2) as a test. Everything is production
// code except the microphone, a fake that delivers audio like a device, and
// the light, the test blink(1): streamMicWithRetry -> captureGate.Writer ->
// Recorder.Writer -> the Deepgram send queue -> ResilientClient ->
// deepgramSocket, dialled by makeDeepgramClientFactory to a local websocket
// peer.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	interfaces "github.com/deepgram/deepgram-go-sdk/v3/pkg/client/interfaces"
	"github.com/dvonthenen/websocket"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/indicator"
	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// deliveringMic delivers chunk bytes every interval, like a capture device,
// until it is held, when it delivers nothing while staying open.
type deliveringMic struct {
	open      atomic.Bool
	held      atomic.Bool
	delivered atomic.Int64
	chunk     int
	interval  time.Duration
}

func (m *deliveringMic) IsOpen() bool  { return m.open.Load() }
func (m *deliveringMic) Start() error  { m.open.Store(true); return nil }
func (m *deliveringMic) Stop() error   { m.open.Store(false); return nil }
func (m *deliveringMic) Close() error  { m.open.Store(false); return nil }
func (m *deliveringMic) Reopen() error { m.open.Store(true); return nil }
func (m *deliveringMic) Stream(w io.Writer) error {
	buf := make([]byte, m.chunk)
	for {
		if !m.open.Load() {
			return errors.New("stream closed")
		}
		if !m.held.Load() {
			if _, err := w.Write(buf); err != nil {
				return err
			}
			m.delivered.Add(1)
		}
		time.Sleep(m.interval)
	}
}

// recordedBytes counts what reaches the recording: the recorder's writer
// wrapped by the test. With dark set, it also counts the writes that reach
// the recording while dark reports the light is not red.
type recordedBytes struct {
	dst        io.Writer
	bytes      atomic.Int64
	dark       func() bool
	darkWrites atomic.Int64
}

func (r *recordedBytes) Write(p []byte) (int, error) {
	if r.dark != nil && r.dark() {
		r.darkWrites.Add(1)
	}
	n, err := r.dst.Write(p)
	r.bytes.Add(int64(n))
	return n, err
}

type micRig struct {
	mic      *deliveringMic
	capture  *captureGate
	device   *pluggableLight
	warnings *warningsRegistry
	dials    *atomic.Int64
	recorded *recordedBytes
	port     int // the local Deepgram peer's port
}

// rigStopTimeout bounds each step of the rig's cleanup, so a failed test
// reports its failure rather than hanging to go test's timeout.
const rigStopTimeout = 10 * time.Second

// stopWithin runs stop, and fails the test if it has not returned within
// rigStopTimeout; it then leaves it running.
func stopWithin(t *testing.T, what string, stop func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), rigStopTimeout)
	defer cancel()
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		stop()
	}()
	select {
	case <-stopped:
	case <-ctx.Done():
		t.Errorf("%s did not stop within %v of the test's end", what, rigStopTimeout)
	}
}

// startMicRig runs the production microphone chain against a local Deepgram
// peer that reads, or that stops reading every connection as a stalled
// network path does, with writeLimit as the Deepgram socket's write limit.
func startMicRig(t *testing.T, peerReads bool, writeLimit time.Duration) *micRig {
	t.Helper()
	var dials atomic.Int64
	// A stalled peer keeps each connection open and unread, with no
	// goroutine of its own, so the test's goroutine count is the client's.
	var stalledMu sync.Mutex
	var stalled []*websocket.Conn
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		dials.Add(1)
		if peerReads {
			defer func() { _ = c.Close() }()
			for {
				if _, _, err := c.ReadMessage(); err != nil {
					return
				}
			}
		}
		if tcp, ok := c.UnderlyingConn().(*net.TCPConn); ok {
			_ = tcp.SetReadBuffer(4096)
		}
		stalledMu.Lock()
		stalled = append(stalled, c)
		stalledMu.Unlock()
	}))
	ctx, cancel := context.WithCancel(context.Background())
	var loops sync.WaitGroup
	mic := &deliveringMic{chunk: 64 << 10, interval: 2 * time.Millisecond}
	t.Cleanup(func() {
		cancel()
		// A held microphone writes nothing, so only closing it ends its
		// Stream, and with it the mic loop.
		_ = mic.Stop()
		stalledMu.Lock()
		for _, c := range stalled {
			_ = c.Close()
		}
		stalledMu.Unlock()
		srv.CloseClientConnections()
		stopWithin(t, "the rig's microphone loop and peer", func() {
			srv.Close()
			loops.Wait()
		})
	})

	cOptions := &interfaces.ClientOptions{Host: "ws://" + srv.Listener.Addr().String(), EnableKeepAlive: true}
	tOptions := &interfaces.LiveTranscriptionOptions{Model: "nova-3", Language: "en-US", Encoding: "linear16", SampleRate: 16000, Channels: 1}
	factory := makeDeepgramClientFactory(ctx, "test-key", cOptions, tOptions, transcriptCallback{}, writeLimit)
	initial, err := factory(ctx)
	if err != nil {
		t.Fatalf("connect the Deepgram client to the local peer: %v", err)
	}
	// log.Printf, not t.Logf: the resilient client's reconnect and the send
	// queue can log after the test ends.
	resilient := transcribe.NewResilientClient(ctx, nil, transcribe.ResilientConfig{BufferSize: 1 << 20, InitialReconnectDelay: 50 * time.Millisecond, MaxReconnectBackoff: 200 * time.Millisecond}, log.Printf, nil)
	resilient.Factory = factory
	resilient.Client = initial
	t.Cleanup(func() { stopWithin(t, "the Deepgram client", func() { _ = resilient.Close() }) })

	rig := &micRig{
		mic:      mic,
		capture:  newCaptureGate(nil, nil, nil),
		device:   &pluggableLight{present: true},
		warnings: newWarningsRegistry(),
		dials:    &dials,
		port:     srv.Listener.Addr().(*net.TCPAddr).Port,
	}
	rig.recorded = &recordedBytes{dark: func() bool { return !rig.device.isRed() }}
	startTestLight(t, rig.device, 100*time.Millisecond, rig.capture, rig.mic, rig.warnings)
	recorder := audio.NewRecorder(t.TempDir())
	originalStarter := streamMicLoopStarter
	streamMicLoopStarter = func(ctx context.Context, loops *sync.WaitGroup, capture *captureGate, streamer micStreamer, writer io.Writer, wait func(time.Duration), logf func(string, ...any), hub *server.Hub) {
		rig.recorded.dst = writer
		originalStarter(ctx, loops, capture, streamer, rig.recorded, wait, logf, hub)
	}
	t.Cleanup(func() { streamMicLoopStarter = originalStarter })
	if err := startMicIfUnpaused(rig.capture, rig.mic.Start); err != nil {
		t.Fatalf("start the microphone: %v", err)
	}
	startMicMonitoring(ctx, &loops, rig.capture, rig.mic, recorder, resilient, func(d time.Duration) { time.Sleep(d) }, log.Printf, nil)
	waitForHardware(t, "the light red while capturing", rig.device.isRed)
	return rig
}

// TestStalledDeepgramKeepsRecording fails if a Deepgram peer that stops
// reading stops the recording, while the control, a peer that reads, keeps it
// going; or if the stalled socket is never replaced.
func TestStalledDeepgramKeepsRecording(t *testing.T) {
	for _, peerReads := range []bool{true, false} {
		name := "control: the peer reads"
		if !peerReads {
			name = "the peer stops reading"
		}
		t.Run(name, func(t *testing.T) {
			rig := startMicRig(t, peerReads, 300*time.Millisecond)
			time.Sleep(time.Second) // fill the socket buffers
			before := rig.recorded.bytes.Load()
			time.Sleep(2 * time.Second)
			after := rig.recorded.bytes.Load()
			if after == before {
				t.Fatalf("no audio reached the recording for 2 s (%d bytes); light red=%t, capture_state=%q", after, rig.device.isRed(), rig.capture.captureState(rig.mic))
			}
			if !rig.device.isRed() || rig.capture.captureState(rig.mic) != captureCapturing {
				t.Fatalf("while recording: light red=%t, capture_state=%q, want red and capturing", rig.device.isRed(), rig.capture.captureState(rig.mic))
			}
			if !peerReads && rig.dials.Load() < 2 {
				t.Fatalf("the stalled Deepgram socket was never replaced: %d connections", rig.dials.Load())
			}
		})
	}
}

// TestSendQueueKeepsRecordingUnderALongWriteLimit fails if the microphone
// loop writes to Deepgram directly rather than through the send queue: with
// the write limit longer than the window watched, a stalled socket then holds
// the loop, and the recording, for the whole limit.
func TestSendQueueKeepsRecordingUnderALongWriteLimit(t *testing.T) {
	rig := startMicRig(t, false, 30*time.Second)
	time.Sleep(time.Second) // fill the socket buffers
	before := rig.recorded.bytes.Load()
	time.Sleep(2 * time.Second)
	if after := rig.recorded.bytes.Load(); after == before {
		t.Fatalf("no audio reached the recording for 2 s (%d bytes) while a Deepgram write waited on its 30 s limit", after)
	}
}

// TestStalledDeepgramSocketsDoNotPileUp is the #65 round 2 review's
// leaking-peer probe: a peer that accepts every connection and stops reading
// each one. It fails if a stalled socket outlives its replacement, so that
// sockets and goroutines grow with every reconnect; the control, a peer that
// reads, keeps one socket.
func TestStalledDeepgramSocketsDoNotPileUp(t *testing.T) {
	for _, peerReads := range []bool{true, false} {
		name := "control: the peer reads"
		if !peerReads {
			name = "the peer stops reading every connection"
		}
		t.Run(name, func(t *testing.T) {
			rig := startMicRig(t, peerReads, 300*time.Millisecond)
			time.Sleep(time.Second)
			goroutinesBefore := runtime.NumGoroutine()
			dialsBefore := rig.dials.Load()
			time.Sleep(4 * time.Second)
			sockets := establishedTo(t, rig.port)
			grown := runtime.NumGoroutine() - goroutinesBefore
			reconnects := rig.dials.Load() - dialsBefore
			t.Logf("over 4 s: %d reconnects, %d established sockets to the peer, goroutines %+d", reconnects, sockets, grown)
			if !peerReads && reconnects < 4 {
				t.Fatalf("only %d reconnects in 4 s; the stall did not replace the socket", reconnects)
			}
			if sockets > 2 || grown > 4 {
				t.Fatalf("after %d reconnects: %d established sockets to the peer and goroutines %+d, want at most 2 and a flat count", reconnects, sockets, grown)
			}
		})
	}
}

// establishedTo counts this host's established TCP connections to local port
// port, from the client side, read from /proc/net/tcp.
func establishedTo(t *testing.T, port int) int {
	t.Helper()
	data, err := os.ReadFile("/proc/net/tcp")
	if err != nil {
		t.Skipf("no /proc/net/tcp to count sockets: %v", err)
	}
	want := fmt.Sprintf(":%04X", port)
	count := 0
	for _, line := range strings.Split(string(data), "\n")[1:] {
		fields := strings.Fields(line)
		// fields: sl local_address rem_address st ...; 01 is ESTABLISHED.
		if len(fields) > 3 && strings.HasSuffix(fields[2], want) && fields[3] == "01" {
			count++
		}
	}
	return count
}

// TestLightGoesOffWhenNoAudioIsRecorded fails if the light stays red, or the
// status says capturing, while no audio reaches the recording, or if both do
// not come back when audio does.
func TestLightGoesOffWhenNoAudioIsRecorded(t *testing.T) {
	rig := startMicRig(t, true, 300*time.Millisecond)
	rig.mic.held.Store(true)
	waitForHardwareWithin(t, 5*time.Second, "the light off while no audio is recorded", func() bool {
		return !rig.device.isRed() && rig.capture.captureState(rig.mic) == captureNoAudio
	})
	rig.mic.held.Store(false)
	waitForHardwareWithin(t, 5*time.Second, "the light red again once audio is recorded", func() bool {
		return rig.device.isRed() && rig.capture.captureState(rig.mic) == captureCapturing
	})
}

// TestAudioAfterAStallIsRecordedOnlyWithTheLightRed fails if audio that
// comes back after a stall turned the light off reaches the recording before
// the light is red again: up to a light check's interval of it was recorded
// with the light off.
func TestAudioAfterAStallIsRecordedOnlyWithTheLightRed(t *testing.T) {
	rig := startMicRig(t, true, 300*time.Millisecond)
	rig.mic.held.Store(true)
	waitForHardwareWithin(t, 5*time.Second, "the light off while no audio is recorded", func() bool {
		return !rig.device.isRed() && rig.capture.captureState(rig.mic) == captureNoAudio
	})
	rig.recorded.darkWrites.Store(0)
	before := rig.recorded.bytes.Load()
	rig.mic.held.Store(false)
	waitForHardwareWithin(t, 5*time.Second, "audio recorded again", func() bool {
		return rig.recorded.bytes.Load() > before && rig.capture.captureState(rig.mic) == captureCapturing
	})
	if dark := rig.recorded.darkWrites.Load(); dark != 0 {
		t.Fatalf("%d buffers of audio reached the recording with the light off after the stall ended", dark)
	}
}

func waitForHardwareWithin(t *testing.T, within time.Duration, description string, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out after %v waiting for %s", within, description)
}

var _ = indicator.ErrNoBlink1
var _ = config.IndicatorLightBlink1
