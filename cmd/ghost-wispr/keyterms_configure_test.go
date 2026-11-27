package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sync"
	"testing"
	"time"

	interfaces "github.com/deepgram/deepgram-go-sdk/v3/pkg/client/interfaces"
	"github.com/dvonthenen/websocket"

	"github.com/sjawhar/ghost-wispr/internal/wake/transcript"
)

// configureEndpoint is a Deepgram live endpoint that records the keyterms of
// its dial and of each Configure message, and answers each Configure with a
// ConfigureSuccess only when the test says.
type configureEndpoint struct {
	mu         sync.Mutex
	query      url.Values
	configured [][]string
	answer     chan string // the type of the next answer to a Configure
}

func startConfigureEndpoint(t *testing.T) (*configureEndpoint, string) {
	t.Helper()
	fake := &configureEndpoint{answer: make(chan string, 8)}
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.mu.Lock()
		fake.query = r.URL.Query()
		fake.mu.Unlock()
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		var writeMu sync.Mutex
		for {
			kind, message, err := c.ReadMessage()
			if err != nil {
				return
			}
			var frame struct {
				Type     string   `json:"type"`
				Keyterms []string `json:"keyterms"`
			}
			if kind != websocket.TextMessage || json.Unmarshal(message, &frame) != nil || frame.Type != "Configure" {
				continue
			}
			fake.mu.Lock()
			fake.configured = append(fake.configured, frame.Keyterms)
			fake.mu.Unlock()
			go func() {
				answer := <-fake.answer
				body, _ := json.Marshal(map[string]any{"type": answer, "request_id": "00000000-0000-4000-8000-000000000001", "code": "CONFIGURE_INTERNAL_ERROR", "description": "x"})
				writeMu.Lock()
				defer writeMu.Unlock()
				_ = c.WriteMessage(websocket.TextMessage, body)
			}()
		}
	}))
	t.Cleanup(srv.Close)
	return fake, "ws://" + srv.Listener.Addr().String()
}

func (f *configureEndpoint) configures() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.configured)
}

func waitConfigures(t *testing.T, f *configureEndpoint, n int) [][]string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(f.configures()) < n {
		if time.Now().After(deadline) {
			t.Fatalf("Deepgram got %d Configure messages %q, want %d", len(f.configures()), f.configures(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
	return f.configures()
}

// dialConfigureEndpoint dials a configureEndpoint as the unit does, with live
// keyterms: the wake phrases, then the joined agents' keys, then INFRA.
func dialConfigureEndpoint(t *testing.T) (*configureEndpoint, *transcript.LiveKeyterms) {
	t.Helper()
	fake, host := startConfigureEndpoint(t)
	keyterms := transcript.NewLiveKeyterms([]string{"Wispr", "Ghost Wispr"}, func() []string { return []string{"INFRA"} })
	factory := makeDeepgramClientFactory(context.Background(), fixedKey("test-key"),
		&interfaces.ClientOptions{Host: host},
		constOpts(&interfaces.LiveTranscriptionOptions{Model: "nova-3", Language: "en-US", Encoding: "linear16", SampleRate: 16000, Channels: 1}),
		transcriptCallback{keyterms: keyterms}, time.Second)
	socket, err := factory(context.Background())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = socket.Close() })
	return fake, keyterms
}

// TestAJoinConfiguresTheLiveStream fails if a dial does not carry the live
// keyterms, if a join does not reach the open Deepgram stream as a Configure
// message with the agent's key, or if a second Configure is sent before
// Deepgram answered the first (Deepgram refuses it, CONFIGURE_ALREADY_PENDING)
// rather than the newest list once it answers.
func TestAJoinConfiguresTheLiveStream(t *testing.T) {
	runParallel(t)
	fake, host := startConfigureEndpoint(t)
	keyterms := transcript.NewLiveKeyterms([]string{"Wispr", "Ghost Wispr"}, func() []string { return []string{"INFRA"} })
	factory := makeDeepgramClientFactory(context.Background(), fixedKey("test-key"),
		&interfaces.ClientOptions{Host: host},
		constOpts(&interfaces.LiveTranscriptionOptions{Model: "nova-3", Language: "en-US", Encoding: "linear16", SampleRate: 16000, Channels: 1}),
		transcriptCallback{keyterms: keyterms}, time.Second)
	socket, err := factory(context.Background())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = socket.Close() })
	fake.mu.Lock()
	dialed := fake.query["keyterm"]
	fake.mu.Unlock()
	if want := []string{"Wispr", "Whisper", "Ghost Wispr", "INFRA"}; !slices.Equal(dialed, want) {
		t.Fatalf("the dial carried keyterms %q, want %q", dialed, want)
	}

	keyterms.SetJoined([]string{"ABC-12"})
	if got, want := waitConfigures(t, fake, 1)[0], []string{"Wispr", "Whisper", "Ghost Wispr", "ABC-12", "INFRA"}; !slices.Equal(got, want) {
		t.Fatalf("the join sent Configure %q, want %q", got, want)
	}

	// Two more changes while the first waits for its answer: nothing more is
	// sent until Deepgram answers, then only the newest list.
	keyterms.SetJoined([]string{"ABC-12", "XYZ-9"})
	keyterms.SetJoined([]string{"XYZ-9"})
	time.Sleep(150 * time.Millisecond)
	if got := fake.configures(); len(got) != 1 {
		t.Fatalf("Deepgram got %q before it answered the first Configure, want it to wait", got)
	}
	fake.answer <- "ConfigureSuccess"
	if got, want := waitConfigures(t, fake, 2)[1], []string{"Wispr", "Whisper", "Ghost Wispr", "XYZ-9", "INFRA"}; !slices.Equal(got, want) {
		t.Fatalf("after the answer Deepgram got %q, want the newest list %q", got, want)
	}

	// A refusal leaves the stream as it was and the next change still goes.
	fake.answer <- "ConfigureFailure"
	time.Sleep(50 * time.Millisecond)
	keyterms.SetJoined(nil)
	if got, want := waitConfigures(t, fake, 3)[2], []string{"Wispr", "Whisper", "Ghost Wispr", "INFRA"}; !slices.Equal(got, want) {
		t.Fatalf("after a refusal the leave sent %q, want %q", got, want)
	}
	time.Sleep(100 * time.Millisecond)
	if got := len(fake.configures()); got != 3 {
		t.Fatalf("Deepgram got %d Configure messages, want 3", got)
	}
}

// TestARefusedConfigureIsSentAgain fails if a list Deepgram refused, with no
// join or leave since, counts as the stream's: the stream keeps its old
// keyterms, so the list must go once more after a short wait, count as the
// stream's once Deepgram takes it, and then not go again for the same join.
func TestARefusedConfigureIsSentAgain(t *testing.T) {
	runParallel(t)
	fake, keyterms := dialConfigureEndpoint(t)
	joined := []string{"Wispr", "Whisper", "Ghost Wispr", "ABC-12", "INFRA"}
	keyterms.SetJoined([]string{"ABC-12"})
	waitConfigures(t, fake, 1)
	fake.answer <- "ConfigureFailure"
	if got := waitConfigures(t, fake, 2)[1]; !slices.Equal(got, joined) {
		t.Fatalf("after a refusal Deepgram got %q, want the refused list %q again", got, joined)
	}
	fake.answer <- "ConfigureSuccess"
	time.Sleep(200 * time.Millisecond)
	keyterms.SetJoined([]string{"ABC-12"})
	time.Sleep(150 * time.Millisecond)
	if got := fake.configures(); len(got) != 2 {
		t.Fatalf("Deepgram got %q after it took the list, want no third Configure", got)
	}
}

// TestASecondRefusalWaitsForTheNextChange fails if a list Deepgram refused
// twice is sent again before a join or a leave (a loop against a Deepgram
// that keeps refusing), or if the next join or leave does not send it again
// because it was sent before.
func TestASecondRefusalWaitsForTheNextChange(t *testing.T) {
	runParallel(t)
	fake, keyterms := dialConfigureEndpoint(t)
	joined := []string{"Wispr", "Whisper", "Ghost Wispr", "ABC-12", "INFRA"}
	keyterms.SetJoined([]string{"ABC-12"})
	waitConfigures(t, fake, 1)
	fake.answer <- "ConfigureFailure"
	waitConfigures(t, fake, 2)
	fake.answer <- "ConfigureFailure"
	// Three times the second a resend waits.
	time.Sleep(3 * time.Second)
	if got := fake.configures(); len(got) != 2 {
		t.Fatalf("after a second refusal Deepgram got %q, want no third Configure before the list changes", got)
	}
	keyterms.SetJoined([]string{"ABC-12"})
	if got := waitConfigures(t, fake, 3)[2]; !slices.Equal(got, joined) {
		t.Fatalf("the next join sent %q, want the refused list %q again", got, joined)
	}
	fake.answer <- "ConfigureSuccess"
}

// TestARefusedConfigureWarnsUntilOneIsTaken fails if GET /api/status does not
// warn once Deepgram refused a join's keyterms, or still warns once Deepgram
// took them.
func TestARefusedConfigureWarnsUntilOneIsTaken(t *testing.T) {
	runParallel(t)
	fake, keyterms := dialConfigureEndpoint(t)
	warnings := newWarningsRegistry()
	keyterms.SetRefusalHook(keytermsRefusedWarning(warnings))
	keyterms.SetJoined([]string{"ABC-12"})
	waitConfigures(t, fake, 1)
	fake.answer <- "ConfigureFailure"
	waitUntil(t, "the refusal's warning", func() bool { return len(warnings.KeysWithPrefix(keytermsRefusedWarningKey)) == 1 })
	waitConfigures(t, fake, 2)
	fake.answer <- "ConfigureSuccess"
	waitUntil(t, "the warning to clear", func() bool { return len(warnings.List()) == 0 })
}
