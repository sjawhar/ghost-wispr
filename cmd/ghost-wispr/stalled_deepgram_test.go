package main

// The #65 review's stalled-peer probe (B2) as a test. Everything is production
// code except the microphone, a fake that delivers audio like a device, and
// the light, the test blink(1): streamMicWithRetry -> captureGate.Writer ->
// Recorder.Writer -> the Deepgram send -> ResilientClient -> deepgramWriter ->
// the Deepgram SDK, connected by makeDeepgramClientFactory to a local
// websocket peer.

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
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
// wrapped by the test.
type recordedBytes struct {
	dst   io.Writer
	bytes atomic.Int64
}

func (r *recordedBytes) Write(p []byte) (int, error) {
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
}

// startMicRig runs the production microphone chain against a local Deepgram
// peer that reads, or that stops reading as a stalled network path does.
func startMicRig(t *testing.T, peerReads bool) *micRig {
	t.Helper()
	stop := make(chan struct{})
	var dials atomic.Int64
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		dials.Add(1)
		defer func() { _ = c.Close() }()
		if peerReads {
			for {
				if _, _, err := c.ReadMessage(); err != nil {
					return
				}
			}
		}
		if tcp, ok := c.UnderlyingConn().(*net.TCPConn); ok {
			_ = tcp.SetReadBuffer(4096)
		}
		<-stop
	}))
	ctx, cancel := context.WithCancel(context.Background())
	var loops sync.WaitGroup
	t.Cleanup(func() {
		cancel()
		close(stop)
		srv.CloseClientConnections()
		srv.Close()
		loops.Wait()
	})

	cOptions := &interfaces.ClientOptions{Host: "ws://" + srv.Listener.Addr().String(), EnableKeepAlive: true}
	tOptions := &interfaces.LiveTranscriptionOptions{Model: "nova-3", Language: "en-US", Encoding: "linear16", SampleRate: 16000, Channels: 1}
	factory := makeDeepgramClientFactory(ctx, "test-key", cOptions, tOptions, transcriptCallback{}, 300*time.Millisecond)
	initial, err := factory(ctx)
	if err != nil {
		t.Fatalf("connect the Deepgram client to the local peer: %v", err)
	}
	// log.Printf, not t.Logf: the resilient client's reconnect and the send
	// queue can log after the test ends.
	resilient := transcribe.NewResilientClient(ctx, nil, transcribe.ResilientConfig{BufferSize: 1 << 20, InitialReconnectDelay: 50 * time.Millisecond, MaxReconnectBackoff: 200 * time.Millisecond}, log.Printf, nil)
	resilient.Factory = factory
	resilient.Client = initial
	t.Cleanup(func() { _ = resilient.Close() })

	rig := &micRig{
		mic:      &deliveringMic{chunk: 64 << 10, interval: 2 * time.Millisecond},
		capture:  newCaptureGate(nil, nil, nil),
		device:   &pluggableLight{present: true},
		warnings: newWarningsRegistry(),
		dials:    &dials,
	}
	rig.recorded = &recordedBytes{}
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
			rig := startMicRig(t, peerReads)
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

// TestLightGoesOffWhenNoAudioIsRecorded fails if the light stays red, or the
// status says capturing, while no audio reaches the recording, or if both do
// not come back when audio does.
func TestLightGoesOffWhenNoAudioIsRecorded(t *testing.T) {
	rig := startMicRig(t, true)
	rig.mic.held.Store(true)
	waitForHardwareWithin(t, 5*time.Second, "the light off while no audio is recorded", func() bool {
		return !rig.device.isRed() && rig.capture.captureState(rig.mic) == captureNoAudio
	})
	rig.mic.held.Store(false)
	waitForHardwareWithin(t, 5*time.Second, "the light red again once audio is recorded", func() bool {
		return rig.device.isRed() && rig.capture.captureState(rig.mic) == captureCapturing
	})
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
