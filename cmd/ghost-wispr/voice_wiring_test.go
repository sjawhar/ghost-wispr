package main

import (
	"context"
	"encoding/json"
	"io"
	"slices"
	"sync"
	"testing"
	"time"

	api "github.com/deepgram/deepgram-go-sdk/v3/pkg/api/listen/v1/websocket/interfaces"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// TestEveryGateClosingTellsTheVoice fails if a pause or a light hold closes
// the capture gate without telling the voice, with the gate's closings count
// after the closing, or if the stamp the transcript reader takes does not say
// whether the gate is open.
func TestEveryGateClosingTellsTheVoice(t *testing.T) {
	capture := newCaptureGate(nil, nil, nil)
	var mu sync.Mutex
	var closed []int
	capture.onClosed = func(n int) {
		mu.Lock()
		defer mu.Unlock()
		closed = append(closed, n)
	}
	if n, open := capture.voiceStamp(); n != 0 || !open {
		t.Fatalf("stamp of an open gate = (%d, %v), want (0, true)", n, open)
	}
	capture.Pause()
	if n, open := capture.voiceStamp(); n != 1 || open {
		t.Fatalf("stamp after a pause = (%d, %v), want (1, false)", n, open)
	}
	capture.Resume()
	waitOpen(t, capture)
	capture.HoldForLight()
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(closed, []int{1, 2}) {
		t.Fatalf("the voice heard closings %v, want [1 2]: the pause, then the light hold", closed)
	}
}

type heardTranscripts struct {
	mu   sync.Mutex
	got  []voice.Transcript
	ends []bool
}

func (h *heardTranscripts) OnTranscript(t *voice.Transcript) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.got = append(h.got, *t)
}

func (h *heardTranscripts) OnUtteranceEnd(closings int, open bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ends = append(h.ends, open)
}

// TestTheVoiceHearsEveryTranscriptWithTheGateStamp fails if a Deepgram result
// reaches the session manager but not the voice, or reaches it without its
// words, its final and endpoint flags, its Finalize flag, or the gate's stamp
// at its arrival.
func TestTheVoiceHearsEveryTranscriptWithTheGateStamp(t *testing.T) {
	capture := newCaptureGate(nil, nil, nil)
	heard := &heardTranscripts{}
	cb := transcriptCallback{voice: heard, stamp: capture.voiceStamp}

	var mr api.MessageResponse
	if err := json.Unmarshal([]byte(`{"is_final":true,"speech_final":true,"from_finalize":true,"channel":{"alternatives":[{"transcript":"Wispr, hello.","words":[
		{"word":"wispr","punctuated_word":"Wispr,","start":1.0,"end":1.3,"speaker":2},
		{"word":"hello","punctuated_word":"hello.","start":1.4,"end":1.8,"speaker":2}]}]}}`), &mr); err != nil {
		t.Fatal(err)
	}
	capture.Pause()
	if err := cb.Message(&mr); err != nil {
		t.Fatal(err)
	}
	if err := cb.UtteranceEnd(&api.UtteranceEndResponse{}); err != nil {
		t.Fatal(err)
	}
	heard.mu.Lock()
	defer heard.mu.Unlock()
	if len(heard.got) != 1 {
		t.Fatalf("the voice heard %d results, want 1", len(heard.got))
	}
	got := heard.got[0]
	if got.Text != "Wispr, hello." || !got.IsFinal || !got.SpeechFinal || !got.FromFinalize || got.Closings != 1 || got.GateOpen {
		t.Errorf("heard %+v; want the text, final, speech final, from finalize, closings 1, gate closed", got)
	}
	want := []voice.Word{{Text: "Wispr,", Speaker: 2, Start: 1.0, End: 1.3}, {Text: "hello.", Speaker: 2, Start: 1.4, End: 1.8}}
	if !slices.Equal(got.Words, want) {
		t.Errorf("words %+v, want %+v", got.Words, want)
	}
	if !slices.Equal(heard.ends, []bool{false}) {
		t.Errorf("utterance ends %v, want one, with the gate closed", heard.ends)
	}
}

// TestAJabraPressStopsTheVoice fails if a press of the Jabra's mute button,
// pausing or resuming, leaves the voice speaking.
func TestAJabraPressStopsTheVoice(t *testing.T) {
	for _, paused := range []bool{false, true} {
		r := newJabraRig(paused)
		button := newJabraMuteButton(r.capture, r.device, r.controls, r.warnings)
		stops := 0
		button.onPress = func() { stops++ }
		button.press(time.Hour)
		if stops != 1 {
			t.Errorf("paused %v: a press stopped the voice %d times, want once", paused, stops)
		}
	}
}

type tapProbe struct {
	mu sync.Mutex
	n  int
}

func (p *tapProbe) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.n += len(b)
	return len(b), nil
}

// TestVoiceAudioOnlyAfterTheGate fails if the voice's tap is not behind the
// capture gate: it must get the microphone's audio while the gate is open,
// and nothing while it is closed.
func TestVoiceAudioOnlyAfterTheGate(t *testing.T) {
	original := streamMicLoopStarter
	t.Cleanup(func() { streamMicLoopStarter = original })
	var passed io.Writer
	streamMicLoopStarter = func(_ context.Context, _ *sync.WaitGroup, _ *captureGate, _ micStreamer, writer io.Writer, _ func(time.Duration), _ func(string, ...any), _ *server.Hub) {
		passed = writer
	}
	capture := newCaptureGate(nil, nil, nil)
	probe := &tapProbe{}
	recorder := audio.NewRecorder(t.TempDir())
	var loops sync.WaitGroup
	startMicMonitoring(t.Context(), &loops, capture, newScriptedMic(), recorder, nil, probe, func(time.Duration) {}, nopLogf, nil)
	if passed == nil {
		t.Fatal("no mic loop started")
	}
	gated := capture.Writer(passed)
	buf := make([]byte, 8000)
	if _, err := gated.Write(buf); err != nil {
		t.Fatalf("write through the open gate: %v", err)
	}
	if probe.n != len(buf) {
		t.Fatalf("the tap got %d bytes through the open gate, want %d", probe.n, len(buf))
	}
	capture.Pause()
	_, _ = gated.Write(buf)
	if probe.n != len(buf) {
		t.Fatalf("the tap got %d bytes, want none more while the gate is closed", probe.n-len(buf))
	}
}

// TestLiveOptionsCarryTheWakeKeyterms fails if the live Deepgram request
// leaves out the wake phrases as keyterms while the voice is on, or sends
// them while it is off.
func TestLiveOptionsCarryTheWakeKeyterms(t *testing.T) {
	cfg := config.Config{DeepgramModel: "nova-3", Voice: config.Voice{Enabled: true, WakePhrases: []string{"Wispr", "Ghost Wispr"}}}
	if got := liveTranscriptionOptions(&cfg, 16000).Keyterm; !slices.Equal(got, []string{"Wispr", "Ghost Wispr"}) {
		t.Errorf("keyterms %q with the voice on, want the wake phrases", got)
	}
	cfg.Voice.Enabled = false
	if got := liveTranscriptionOptions(&cfg, 16000).Keyterm; len(got) != 0 {
		t.Errorf("keyterms %q with the voice off, want none", got)
	}
}
