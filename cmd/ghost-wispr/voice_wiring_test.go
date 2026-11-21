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

// TestVoiceTapDropsAWriteThatPassedTheGateBeforeAPause fails if a recorder
// write that began before a pause, but finishes after a resume, reaches the
// voice tap after GateClosed emptied it. A new exchange must not receive room
// audio from before the latest closing.
func TestVoiceTapDropsAWriteThatPassedTheGateBeforeAPause(t *testing.T) {
	capture := newCaptureGate(nil, nil, nil)
	capture.writeWait = 10 * time.Millisecond
	tap := voice.NewTap()
	tap.SetRate(voice.TapRate)
	capture.onClosed = func(int) { tap.Reset() }

	dst := &blockingWriter{entered: make(chan struct{}, 1), release: make(chan struct{})}
	writer := capture.Writer(tappedWriter{dst: dst, tap: tap})
	pcm := make([]byte, 2*voice.TapRate/4)
	written := make(chan error, 1)
	go func() {
		_, err := writer.Write(pcm)
		written <- err
	}()
	<-dst.entered

	capture.Pause()
	capture.Resume()
	waitOpen(t, capture)
	close(dst.release)
	if err := <-written; err != nil {
		t.Fatalf("the in-flight write: %v", err)
	}

	frames := tap.Open(2 * time.Second)
	select {
	case f := <-frames:
		t.Fatalf("a new exchange got %d samples from before the pause", len(f.PCM))
	default:
	}
}

type solBlockedWriter struct {
	entered chan struct{}
	release chan struct{}
}

func (w solBlockedWriter) Write(p []byte) (int, error) {
	close(w.entered)
	<-w.release
	return len(p), nil
}

// TestSolInflightTapCannotReplayBeforePause keeps the real capture gate,
// wiring, Supervisor and Tap: a destination write held across a pause cannot
// repopulate the reset tap before the next exchange opens it.
func TestSolInflightTapCannotReplayBeforePause(t *testing.T) {
	for _, inflight := range []bool{false, true} {
		name := "control: write completed before pause"
		if inflight {
			name = "pause while recorder write in flight"
		}
		t.Run(name, func(t *testing.T) {
			tap := voice.NewTap()
			capture := newCaptureGate(nil, nil, nil)
			out := audio.NewOutput(audio.OutputConfig{Open: noSpeaker})
			t.Cleanup(func() { _ = out.Close() })
			sup := voice.NewSupervisor(&voice.SupervisorConfig{
				Concierge: stubConcierge{},
				Speaker:   voice.SpeakerOf(out),
				Tap:       tap,
				Red:       func() bool { return !capture.isClosed() },
				Session:   func() string { return "meeting-1" },
			})
			t.Cleanup(sup.Close)
			link := &voiceLink{}
			link.set(sup)
			wireVoiceToCapture(capture, link, nil)
			blocked := solBlockedWriter{make(chan struct{}), make(chan struct{})}
			writer := capture.Writer(tappedWriter{dst: blocked, tap: tap})
			data := make([]byte, 8000)
			for i := range len(data) / 2 {
				data[2*i], data[2*i+1] = 0x34, 0x12
			}
			done := make(chan error, 1)
			go func() {
				_, err := writer.Write(data)
				done <- err
			}()
			<-blocked.entered
			if inflight {
				capture.Pause()
			}
			close(blocked.release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if !inflight {
				capture.Pause()
			}
			capture.Resume()
			waitOpen(t, capture)
			frames := tap.Open(time.Second)
			select {
			case frame := <-frames:
				t.Fatalf("new exchange received %d pre-pause samples, first=%#x", len(frame.PCM), frame.PCM[0])
			case <-time.After(25 * time.Millisecond):
				t.Log("no pre-pause frame was replayed")
			}
			tap.CloseOut()
		})
	}
}

// unstampedTapWriter is the old tappedWriter: its tap write has no record of
// the capture generation that admitted its destination write.
type unstampedTapWriter struct{ dst, tap io.Writer }

func (w unstampedTapWriter) Write(p []byte) (int, error) {
	n, err := w.dst.Write(p)
	_, _ = w.tap.Write(p)
	return n, err
}

// TestVoiceTapDropsAFrameAdmittedBeforeALightCheckClosed puts the gate's
// checkLight after admission. A closing and resume while that check blocks
// must drop the old frame and preserve the new frame after the resume.
func TestVoiceTapDropsAFrameAdmittedBeforeALightCheckClosed(t *testing.T) {
	for _, tc := range []struct {
		name string
		new  func(io.Writer, io.Writer) io.Writer
		want int16
	}{
		{"control: an unstamped tap leaks the old frame", func(dst, tap io.Writer) io.Writer {
			return unstampedTapWriter{dst: dst, tap: tap}
		}, 0x1234},
		{"the stamped tap drops the old frame", func(dst, tap io.Writer) io.Writer {
			return tappedWriter{dst: dst, tap: tap}
		}, 0x5678},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tap := voice.NewTap()
			capture := newCaptureGate(nil, nil, nil)
			capture.writeWait = 10 * time.Millisecond
			out := audio.NewOutput(audio.OutputConfig{Open: noSpeaker})
			t.Cleanup(func() { _ = out.Close() })
			sup := voice.NewSupervisor(&voice.SupervisorConfig{
				Concierge: stubConcierge{},
				Speaker:   voice.SpeakerOf(out),
				Tap:       tap,
				Red:       func() bool { return !capture.isClosed() },
				Session:   func() string { return "meeting-1" },
			})
			t.Cleanup(sup.Close)
			link := &voiceLink{}
			link.set(sup)
			wireVoiceToCapture(capture, link, nil)

			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			capture.checkLight = func() {
				once.Do(func() {
					close(entered)
					<-release
				})
			}
			writer := capture.Writer(tc.new(io.Discard, tap))
			old := make([]byte, 8000)
			for i := range len(old) / 2 {
				old[2*i], old[2*i+1] = 0x34, 0x12
			}
			wrote := make(chan error, 1)
			go func() {
				_, err := writer.Write(old)
				wrote <- err
			}()
			<-entered
			capture.Pause()
			capture.Resume()
			waitOpen(t, capture)
			close(release)
			if err := <-wrote; err != nil {
				t.Fatalf("the admitted write: %v", err)
			}

			fresh := make([]byte, 8000)
			for i := range len(fresh) / 2 {
				fresh[2*i], fresh[2*i+1] = 0x78, 0x56
			}
			if _, err := writer.Write(fresh); err != nil {
				t.Fatalf("the fresh write: %v", err)
			}
			frames := tap.Open(time.Second)
			t.Cleanup(tap.CloseOut)
			frame := <-frames
			if got := frame.PCM[0]; got != tc.want {
				t.Fatalf("the replay starts %#x, want %#x", got, tc.want)
			}
		})
	}
}

// TestLiveOptionsCarryTheWakeKeyterms fails if the live Deepgram request
// leaves out the wake phrases as keyterms while a voice can run (a speaker,
// whatever voice.enabled says, since a person turns it on with no restart),
// or sends them with no speaker.
func TestLiveOptionsCarryTheWakeKeyterms(t *testing.T) {
	cfg := config.Config{DeepgramModel: "nova-3", SpeakerEnabled: true, Voice: config.Voice{WakePhrases: []string{"Wispr", "Ghost Wispr"}}}
	if got := liveTranscriptionOptions(&cfg, 16000, nil).Keyterm; !slices.Equal(got, []string{"Wispr", "Ghost Wispr"}) {
		t.Errorf("keyterms %q with a speaker and the voice off, want the wake phrases", got)
	}
	cfg.SpeakerEnabled = false
	if got := liveTranscriptionOptions(&cfg, 16000, nil).Keyterm; len(got) != 0 {
		t.Errorf("keyterms %q with no speaker, want none", got)
	}
}

// noSpeaker is an Output's Open that opens no device: a test's Output never
// reaches a real speaker (ALSA's "default" can be a sound server elsewhere).
func noSpeaker(string) (audio.PlaybackStream, int, error) {
	return nil, 0, context.Canceled
}
