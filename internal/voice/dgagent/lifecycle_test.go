package dgagent

// The session's life from Welcome to Close, and the paths a review found no
// test for: each test here was run against a mutant of the code it names
// and failed on it.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dvonthenen/websocket"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// TestSettingsWaitForWelcome fails if the session sends anything before
// Deepgram's Welcome, which the docs forbid.
func TestSettingsWaitForWelcome(t *testing.T) {
	var early, welcomed atomic.Bool
	settingsIn := make(chan struct{}, 1)
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = ws.Close() }()
		readerDone := make(chan struct{})
		go func() {
			defer close(readerDone)
			for {
				_, data, err := ws.ReadMessage()
				if err != nil {
					return
				}
				if !welcomed.Load() {
					early.Store(true)
				}
				if strings.Contains(string(data), `"Settings"`) {
					select {
					case settingsIn <- struct{}{}:
					default:
					}
				}
			}
		}()
		time.Sleep(150 * time.Millisecond)
		welcomed.Store(true)
		_ = ws.WriteMessage(websocket.TextMessage, []byte(`{"type":"Welcome","request_id":"x"}`))
		select {
		case <-settingsIn:
		case <-time.After(3 * time.Second):
			return
		}
		_ = ws.WriteMessage(websocket.TextMessage, []byte(`{"type":"SettingsApplied"}`))
		<-readerDone
	}))
	t.Cleanup(srv.Close)
	agent, err := New(&Config{APIKey: "k", URL: "ws" + strings.TrimPrefix(srv.URL, "http")})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := agent.Open(context.Background(), &voice.Exchange{Out: newFakeOutput(), Tools: tools(nil)})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_ = sess.Close()
	if early.Load() {
		t.Fatal("the session sent a message before Deepgram's Welcome")
	}
}

// TestOpenGivesUpWhenItsContextEnds fails if Open outlives its context while
// Deepgram never applies the Settings, or hides that the context ended.
func TestOpenGivesUpWhenItsContextEnds(t *testing.T) {
	s := startStandIn(t)
	agent, err := New(&Config{APIKey: "k", URL: s.url})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		sess, err := agent.Open(ctx, &voice.Exchange{Out: newFakeOutput(), Tools: tools(nil)})
		if sess != nil {
			_ = sess.Close()
		}
		done <- err
	}()
	c := s.accept(t)
	c.expect(t, "Settings")
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Open = %v, want an error that is its context's", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Open outlived its context while Deepgram never applied the Settings")
	}
}

// TestOpenNamesTheRefusal fails if Open hides the HTTP status Deepgram
// refused the connection with, so a bad key reads like any failure.
func TestOpenNamesTheRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "INVALID_AUTH", http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	agent, err := New(&Config{APIKey: "k", URL: "ws" + strings.TrimPrefix(srv.URL, "http")})
	if err != nil {
		t.Fatal(err)
	}
	_, err = agent.Open(context.Background(), &voice.Exchange{Out: newFakeOutput(), Tools: tools(nil)})
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("Open = %v, want an error naming the 401", err)
	}
}

// TestSayEndsWhenTheConnectionDrops fails if a Say waiting for its echo
// keeps waiting once the connection has gone, or a later Say is sent.
func TestSayEndsWhenTheConnectionDrops(t *testing.T) {
	s := startStandIn(t)
	sess, c, _ := open(t, s, Config{}, voice.Exchange{Out: newFakeOutput(), Tools: tools(nil)})
	done := sayAsync(sess, "A fact.")
	c.expect(t, "InjectAgentMessage")
	c.drop()
	if err := returned(t, done); !errors.Is(err, ErrDisconnected) {
		t.Fatalf("Say = %v, want ErrDisconnected", err)
	}
	if err := sess.Say(context.Background(), "Another.", voice.Fact); !errors.Is(err, ErrDisconnected) {
		t.Fatalf("Say after the drop = %v, want ErrDisconnected", err)
	}
}

// TestSayGivesUpWithItsContext fails if Say outlives its context.
func TestSayGivesUpWithItsContext(t *testing.T) {
	s := startStandIn(t)
	sess, c, _ := open(t, s, Config{}, voice.Exchange{Out: newFakeOutput(), Tools: tools(nil)})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- sess.Say(ctx, "Never echoed.", voice.Fact) }()
	c.expect(t, "InjectAgentMessage")
	if err := returned(t, done); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Say = %v, want its context's error", err)
	}
}

// TestAReplyAfterUserSpeechPlaysAfterAnInterrupt fails if, after an
// Interrupt, the audio of the next reply is dropped as the rest of the
// interrupted one when Deepgram's UserStartedSpeaking came between them and
// no AgentAudioDone did. The next reply's audio comes before its text, as
// Deepgram may send it, so only UserStartedSpeaking can end the drop.
func TestAReplyAfterUserSpeechPlaysAfterAnInterrupt(t *testing.T) {
	setSpeechGap(t, time.Minute)
	s := startStandIn(t)
	out := newFakeOutput()
	sess, c, _ := open(t, s, Config{}, voice.Exchange{Out: out, Tools: tools(nil)})
	first := tone(OutputRate, 40*time.Millisecond, 100)
	firstDone := sayAsync(sess, "First reply.")
	c.expect(t, "InjectAgentMessage")
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "First reply."})
	c.sendAudio(first)
	p := out.nextPlayback(t)
	p.waitBytes(t, len(first))
	waitEvent(t, sess, voice.EventSpeechStarted)
	sess.Interrupt()
	_ = returned(t, firstDone)
	waitEvent(t, sess, voice.EventSpeechEnded)
	c.sendAudio(tone(OutputRate, 40*time.Millisecond, 300))
	c.send(map[string]any{"type": "UserStartedSpeaking"})
	next := tone(OutputRate, 20*time.Millisecond, 400)
	// The next reply: the voice's own words, their audio before their echo.
	_ = sayAsync(sess, "Next words.")
	c.expect(t, "InjectAgentMessage")
	c.sendAudio(next)
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "Next words."})
	p2 := out.nextPlayback(t)
	p2.waitBytes(t, len(next))
	if got := p2.written(); !bytes.Equal(got, next) {
		t.Fatalf("the reply after UserStartedSpeaking played %d bytes, want its %d", len(got), len(next))
	}
}

// TestInterruptStopsSpeechTheOutputHolds fails if an Interrupt while the
// Output is full, the usual case live (Deepgram sends speech faster than it
// plays), leaves the voice speaking or plays the queued rest of the reply.
func TestInterruptStopsSpeechTheOutputHolds(t *testing.T) {
	setSpeechGap(t, time.Minute)
	s := startStandIn(t)
	out := newFakeOutput()
	out.blockWrites = true
	sess, c, _ := open(t, s, Config{}, voice.Exchange{Out: out, Tools: tools(nil)})
	first := sayAsync(sess, "First reply.")
	c.expect(t, "InjectAgentMessage")
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "First reply."})
	c.sendAudio(tone(OutputRate, 40*time.Millisecond, 100))
	out.nextPlayback(t)
	waitEvent(t, sess, voice.EventSpeechStarted)
	c.sendAudio(tone(OutputRate, 40*time.Millisecond, 200))
	c.sendAudio(tone(OutputRate, 40*time.Millisecond, 300))
	c.send(map[string]any{"type": "ConversationText", "role": "user", "content": "marker"})
	waitEvent(t, sess, voice.EventUserTurn) // the audio before it is queued
	sess.Interrupt()
	_ = returned(t, first)
	waitEvent(t, sess, voice.EventSpeechEnded)
	select {
	case <-out.begun:
		t.Fatal("after Interrupt the session began another playback with the queued rest of the reply")
	case <-time.After(300 * time.Millisecond):
	}
}

// slowOutput's Write takes 50 ms to see its context end, and counts every
// call made to it after closedAt is set.
type slowOutput struct {
	entered  chan struct{}
	active   atomic.Int32
	closedAt atomic.Int64
	late     atomic.Int32
}

func (o *slowOutput) note() {
	if o.closedAt.Load() != 0 {
		o.late.Add(1)
	}
}

func (o *slowOutput) Begin() voice.Playback { o.note(); return o }

func (o *slowOutput) Write(ctx context.Context, _ []byte, _ audio.AudioFormat) error {
	o.note()
	o.active.Add(1)
	defer o.active.Add(-1)
	select {
	case o.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	time.Sleep(50 * time.Millisecond)
	return ctx.Err()
}

func (o *slowOutput) Drain(context.Context) error { o.note(); return nil }

// TestCloseLeavesTheOutputAlone fails if Close returns while a write to the
// Output still runs, or the session calls the Output after Close returned.
func TestCloseLeavesTheOutputAlone(t *testing.T) {
	setSpeechGap(t, time.Minute)
	s := startStandIn(t)
	out := &slowOutput{entered: make(chan struct{}, 1)}
	sess, c, _ := open(t, s, Config{}, voice.Exchange{Out: out, Tools: tools(nil)})
	done := sayAsync(sess, "Close me.")
	c.expect(t, "InjectAgentMessage")
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "Close me."})
	c.sendAudio(tone(OutputRate, 100*time.Millisecond, 100))
	select {
	case <-out.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("no write began")
	}
	start := time.Now()
	_ = sess.Close()
	_ = done
	out.closedAt.Store(time.Now().UnixNano())
	if n := out.active.Load(); n != 0 {
		t.Fatalf("Close returned after %v with %d Output write(s) still running", time.Since(start), n)
	}
	time.Sleep(100 * time.Millisecond)
	if n := out.late.Load(); n != 0 {
		t.Fatalf("the session called the Output %d times after Close returned", n)
	}
}

// lockedBuf is a log sink safe for the session's goroutines.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (w *lockedBuf) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *lockedBuf) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

// TestCloseReportsNoError fails if Close is reported, or logged, as a
// connection that was lost.
func TestCloseReportsNoError(t *testing.T) {
	s := startStandIn(t)
	var logs lockedBuf
	sess, _, _ := open(t, s, Config{Logger: slog.New(slog.NewTextHandler(&logs, nil))}, voice.Exchange{Out: newFakeOutput(), Tools: tools(nil)})
	_ = sess.Close()
	for _, e := range waitEventsClosed(t, sess) {
		if e.Kind == voice.EventErr {
			t.Errorf("Close reported Err %v", e.Err)
		}
	}
	if strings.Contains(logs.String(), "connection ended") {
		t.Errorf("Close logged a lost connection: %s", logs.String())
	}
}

// TestInterruptSendsOnlyAudioHeldSinceTheRoomWasLastHeard fails if room
// audio held during an earlier utterance, which nobody interrupted, goes to
// Deepgram with the words that interrupt a later one.
func TestInterruptSendsOnlyAudioHeldSinceTheRoomWasLastHeard(t *testing.T) {
	setSpeechGap(t, time.Minute)
	s := startStandIn(t)
	tap := make(chan audio.Frame, 16)
	cfg := Config{echoTail: -1} // the room after the first reply is heard at once
	sess, c, _ := open(t, s, cfg, voice.Exchange{Out: newFakeOutput(), Tap: tap, Tools: tools(nil)})
	frame := func(v int16) audio.Frame { return audio.Frame{PCM: []int16{v, v}, Rate: 16000, Channels: 1} }
	first := sayAsync(sess, "First reply.")
	c.expect(t, "InjectAgentMessage")
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "First reply."})
	c.sendAudio(tone(OutputRate, 20*time.Millisecond, 100))
	waitEvent(t, sess, voice.EventSpeechStarted)
	tap <- frame(5)
	if got := c.nextAudio(t); !bytes.Equal(got, make([]byte, 4)) {
		t.Fatalf("got %v, want silence", got)
	}
	c.send(map[string]any{"type": "AgentAudioDone"})
	if err := returned(t, first); err != nil {
		t.Fatalf("Say of first = %v", err)
	}
	waitEvent(t, sess, voice.EventSpeechEnded)
	tap <- frame(6)
	if got := c.nextAudio(t); !bytes.Equal(got, pcm16(6, 6)) {
		t.Fatalf("got %v, want the live audio", got)
	}
	second := sayAsync(sess, "Second reply.")
	c.expect(t, "InjectAgentMessage")
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "Second reply."})
	c.sendAudio(tone(OutputRate, 20*time.Millisecond, 100))
	waitEvent(t, sess, voice.EventSpeechStarted)
	tap <- frame(7)
	if got := c.nextAudio(t); !bytes.Equal(got, make([]byte, 4)) {
		t.Fatalf("got %v, want silence", got)
	}
	sess.Interrupt()
	_ = returned(t, second)
	tap <- frame(8)
	var after []byte
	for !bytes.HasSuffix(after, pcm16(8, 8)) {
		after = append(after, c.nextAudio(t)...)
	}
	if !bytes.Equal(after, pcm16(7, 7, 8, 8)) {
		t.Fatalf("after Interrupt Deepgram got %v, want only the audio held in this utterance, then the live audio", after)
	}
}

// TestCloseCancelsRunningToolCalls fails if a tool call keeps running after
// Close.
func TestCloseCancelsRunningToolCalls(t *testing.T) {
	s := startStandIn(t)
	stopped := make(chan struct{})
	runner := tools(func(ctx context.Context, _ voice.ToolCall) (json.RawMessage, error) {
		<-ctx.Done()
		close(stopped)
		return nil, ctx.Err()
	})
	sess, c, _ := open(t, s, Config{}, voice.Exchange{Out: newFakeOutput(), Tools: runner})
	c.send(answerCall("fc_9", "x"))
	waitEvent(t, sess, voice.EventToolCall)
	_ = sess.Close()
	select {
	case <-stopped:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("a tool call kept running 100 ms after Close")
	}
}

// TestDeliverStopsTheToolRun fails if the Concierge keeps running a call
// that Deliver answers, while Deliver's words wait behind another
// injection.
func TestDeliverStopsTheToolRun(t *testing.T) {
	s := startStandIn(t)
	stopped := make(chan struct{})
	runner := tools(func(ctx context.Context, _ voice.ToolCall) (json.RawMessage, error) {
		<-ctx.Done()
		close(stopped)
		return answerOf("Too late.", ""), nil
	})
	sess, c, _ := open(t, s, Config{}, voice.Exchange{Out: newFakeOutput(), Tools: runner})
	said := sayAsync(sess, "A held fact.") // holds the session's injection until echoed
	c.expect(t, "InjectAgentMessage")
	c.send(answerCall("fc_10", "q"))
	waitEvent(t, sess, voice.EventToolCall)
	delivered := make(chan error, 1)
	go func() { delivered <- sess.Deliver(context.Background(), "fc_10", answerOf("It is done.", "")) }()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("the Concierge kept running the call Deliver answers")
	}
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "A held fact."})
	c.sendAudio(tone(OutputRate, 20*time.Millisecond, 100))
	c.send(map[string]any{"type": "AgentAudioDone"})
	if err := returned(t, said); err != nil {
		t.Fatalf("Say: %v", err)
	}
	c.expect(t, "InjectAgentMessage")
	c.expect(t, "FunctionCallResponse")
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "It is done."})
	if err := returned(t, delivered); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
}

// TestBadArgumentsAnswerWithAnError fails if a call whose arguments are not
// JSON reaches the Concierge, or leaves Deepgram without an answer.
func TestBadArgumentsAnswerWithAnError(t *testing.T) {
	s := startStandIn(t)
	ran := make(chan voice.ToolCall, 1)
	runner := tools(func(_ context.Context, call voice.ToolCall) (json.RawMessage, error) {
		ran <- call
		return answerOf("", ""), nil
	})
	_, c, _ := open(t, s, Config{}, voice.Exchange{Out: newFakeOutput(), Tools: runner})
	c.send(map[string]any{"type": "FunctionCallRequest", "functions": []any{
		map[string]any{"id": "fc_11", "name": voice.AnswerTool, "arguments": `{"question":`, "client_side": true},
	}})
	resp := c.expect(t, "FunctionCallResponse")
	raw, _ := resp["content"].(string)
	var content map[string]any
	if err := json.Unmarshal([]byte(raw), &content); err != nil || content["error"] == nil {
		t.Fatalf("FunctionCallResponse content = %q, want an error", raw)
	}
	select {
	case call := <-ran:
		t.Fatalf("the Concierge ran %+v with arguments that are not JSON", call)
	default:
	}
}

// TestCloseIsQuickWhenNobodyReadsEvents fails if Close waits on a reader
// stuck sending to a full events channel nobody reads.
func TestCloseIsQuickWhenNobodyReadsEvents(t *testing.T) {
	s := startStandIn(t)
	sess, c, _ := open(t, s, Config{}, voice.Exchange{Out: newFakeOutput(), Tools: tools(nil)})
	for i := range eventBuffer + 8 {
		c.send(map[string]any{"type": "ConversationText", "role": "user", "content": strings.Repeat("x", i+1)})
	}
	deadline := time.Now().Add(3 * time.Second)
	for len(sess.Events()) < cap(sess.Events()) {
		if time.Now().After(deadline) {
			t.Fatalf("the events never filled: %d of %d", len(sess.Events()), cap(sess.Events()))
		}
		time.Sleep(5 * time.Millisecond)
	}
	closed := make(chan struct{})
	start := time.Now()
	go func() { _ = sess.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		go func() {
			for range sess.Events() { // let Close finish so the failure is reported
			}
		}()
		t.Fatal("Close did not return within 1 s while nobody read the events")
	}
	if took := time.Since(start); took > 100*time.Millisecond {
		t.Fatalf("Close took %v while nobody read the events, want at most 100 ms", took)
	}
	waitEventsClosed(t, sess)
}

// TestLatencyReportsLogOnlyNumbers fails if a latency report is logged as
// Deepgram sent it, so a field Deepgram adds with words in it would be
// logged too.
func TestLatencyReportsLogOnlyNumbers(t *testing.T) {
	s := startStandIn(t)
	var logs lockedBuf
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	sess, c, _ := open(t, s, Config{Logger: logger}, voice.Exchange{Out: newFakeOutput(), Tools: tools(nil)})
	c.send(map[string]any{"type": "LatencyReport", "total_latency": 0.5, "note": "ROOM-WORDS"})
	c.send(map[string]any{"type": "ConversationText", "role": "user", "content": "after"})
	waitEvent(t, sess, voice.EventUserTurn)
	_ = sess.Close()
	if got := logs.String(); !strings.Contains(got, "total_latency=0.5") || strings.Contains(got, "ROOM-WORDS") {
		t.Fatalf("the latency log = %s, want total_latency=0.5 and no text fields", got)
	}
}

// TestAnEmptyResultIsNotSpoken fails if a tool that returns nothing leaves
// Deepgram an answer that is not a JSON object saying nothing was spoken.
func TestAnEmptyResultIsNotSpoken(t *testing.T) {
	s := startStandIn(t)
	runner := tools(func(context.Context, voice.ToolCall) (json.RawMessage, error) { return nil, nil })
	_, c, _ := open(t, s, Config{}, voice.Exchange{Out: newFakeOutput(), Tools: runner})
	c.send(map[string]any{"type": "FunctionCallRequest", "functions": []any{
		map[string]any{"id": "fc_e", "name": voice.AnswerTool, "arguments": "", "client_side": true},
	}})
	if content := responseContent(t, c.expect(t, "FunctionCallResponse")); content["answered"] != false {
		t.Fatalf("FunctionCallResponse content = %v, want answered false", content)
	}
}

// TestAResultThatIsNotAnAnswerIsLogged fails if an answer tool result that
// is not an AnswerResult is handed over, or passes with no warning in the
// log, so a Concierge that answers in the wrong shape goes unseen.
func TestAResultThatIsNotAnAnswerIsLogged(t *testing.T) {
	s := startStandIn(t)
	var logs lockedBuf
	runner := tools(func(context.Context, voice.ToolCall) (json.RawMessage, error) {
		return json.RawMessage(`"ABC-12 is in review."`), nil
	})
	_, c, _ := open(t, s, Config{Logger: slog.New(slog.NewTextHandler(&logs, nil))}, voice.Exchange{Out: newFakeOutput(), Tools: runner})
	c.send(map[string]any{"type": "FunctionCallRequest", "functions": []any{
		map[string]any{"id": "fc_s", "name": voice.AnswerTool, "arguments": `{"question":"What is ABC-12?"}`, "client_side": true},
	}})
	if content := responseContent(t, c.expect(t, "FunctionCallResponse")); content["answered"] != false {
		t.Fatalf("FunctionCallResponse content = %v, want answered false", content)
	}
	if got := logs.String(); !strings.Contains(got, "level=WARN") || !strings.Contains(got, "the answer is not an AnswerResult") {
		t.Fatalf("the log = %q, want a warning that the answer is not an AnswerResult", got)
	}
}

// TestAQuestionDeepgramDidNotHearGoesToItAsText fails if the asker's words
// at the wake do not reach Deepgram as the user's text when it has heard no
// turn of the room's after the session opened. The end-to-end proof at
// ace991b saw the live transcript lag the mic by 10 s: the question's audio
// had left the tap before the session opened, Deepgram heard silence, and
// the exchange ended with nothing said.
func TestAQuestionDeepgramDidNotHearGoesToItAsText(t *testing.T) {
	s := startStandIn(t)
	_, c, _ := open(t, s, Config{askerWait: 100 * time.Millisecond}, voice.Exchange{
		AskerTurn: " what is happening with ABC-12? ", Out: newFakeOutput(), Tools: tools(nil),
	})
	m := c.expect(t, "InjectUserMessage")
	if m["content"] != "what is happening with ABC-12?" {
		t.Fatalf("InjectUserMessage = %v, want the asker's words as its content", m)
	}
}

// TestAQuestionDeepgramHeardIsNotSentAgain fails if the asker's words go to
// Deepgram as text after it heard the room's turn start, or when the wake
// had no words after it: the question would be answered twice, or an empty
// message would close the session.
func TestAQuestionDeepgramHeardIsNotSentAgain(t *testing.T) {
	s := startStandIn(t)
	_, c, _ := open(t, s, Config{askerWait: 200 * time.Millisecond}, voice.Exchange{
		AskerTurn: "what is happening with ABC-12?", Out: newFakeOutput(), Tools: tools(nil),
	})
	c.send(map[string]any{"type": "UserStartedSpeaking"})
	c.quiet(t, 600*time.Millisecond, "InjectUserMessage")

	_, c, _ = open(t, s, Config{askerWait: 100 * time.Millisecond}, voice.Exchange{AskerTurn: "  ", Out: newFakeOutput(), Tools: tools(nil)})
	c.quiet(t, 400*time.Millisecond, "InjectUserMessage")
}

// TestRoomAudioAtAnotherRateIsDropped fails if room audio that is not 16 kHz
// mono reaches Deepgram, which was told to expect 16 kHz.
func TestRoomAudioAtAnotherRateIsDropped(t *testing.T) {
	s := startStandIn(t)
	tap := make(chan audio.Frame, 4)
	_, c, _ := open(t, s, Config{}, voice.Exchange{Out: newFakeOutput(), Tap: tap, Tools: tools(nil)})
	tap <- audio.Frame{PCM: []int16{9, 9}, Rate: 48000, Channels: 1}
	tap <- audio.Frame{PCM: []int16{8, 8}, Rate: 16000, Channels: 2}
	tap <- audio.Frame{PCM: []int16{7, 7}, Rate: 16000, Channels: 1}
	if got := c.nextAudio(t); !bytes.Equal(got, pcm16(7, 7)) {
		t.Fatalf("Deepgram got %v first, want only the 16 kHz mono audio", got)
	}
}

// TestTheAgentDrivesItsOwnTurn fails if voice C does not tell the
// Supervisor that Deepgram drives its turns, so the Supervisor would hand
// the Concierge the turn as well and both would answer.
func TestTheAgentDrivesItsOwnTurn(t *testing.T) {
	agent, err := New(&Config{APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	var v voice.Voice = agent
	d, ok := v.(voice.TurnDriver)
	if !ok || !d.DrivesTurn() {
		t.Fatal("voice C is no TurnDriver that drives its turn")
	}
}

// TestTheVoicesLastWordsDoNotReachDeepgram fails if the room buffer that
// comes right after the voice's speech ends, recorded while the speaker
// still played it, goes to Deepgram as if someone in the room said it.
func TestTheVoicesLastWordsDoNotReachDeepgram(t *testing.T) {
	setSpeechGap(t, time.Minute)
	s := startStandIn(t)
	tap := make(chan audio.Frame, 4)
	sess, c, _ := open(t, s, Config{echoTail: 200 * time.Millisecond}, voice.Exchange{Out: newFakeOutput(), Tap: tap, Tools: tools(nil)})
	done := sayAsync(sess, "Voice words.")
	c.expect(t, "InjectAgentMessage")
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "Voice words."})
	c.sendAudio(tone(OutputRate, 20*time.Millisecond, 100))
	c.send(map[string]any{"type": "AgentAudioDone"})
	if err := returned(t, done); err != nil {
		t.Fatalf("Say = %v", err)
	}
	waitEvent(t, sess, voice.EventSpeechEnded)
	tap <- audio.Frame{PCM: []int16{5, 5}, Rate: 16000, Channels: 1}
	if got := c.nextAudio(t); !bytes.Equal(got, make([]byte, 4)) {
		t.Fatalf("right after the voice ended Deepgram got %v, want silence", got)
	}
	time.Sleep(300 * time.Millisecond)
	tap <- audio.Frame{PCM: []int16{6, 6}, Rate: 16000, Channels: 1}
	if got := c.nextAudio(t); !bytes.Equal(got, pcm16(6, 6)) {
		t.Fatalf("once the echo had passed Deepgram got %v, want the room", got)
	}
}

// TestADisconnectKeepsNoCloseText fails if the Err event of a connection
// Deepgram closed carries the close frame's text, which the Supervisor
// logs, or loses its close code.
func TestADisconnectKeepsNoCloseText(t *testing.T) {
	const words = "ROOM-WORDS-IN-CLOSE"
	s := startStandIn(t)
	sess, c, _ := open(t, s, Config{}, voice.Exchange{Out: newFakeOutput(), Tools: tools(nil)})
	c.dropWith(websocket.CloseInternalServerErr, words)
	ev := waitEvent(t, sess, voice.EventErr)
	if strings.Contains(ev.Err.Error(), words) {
		t.Errorf("Err event = %q, want it without the close frame's text", ev.Err)
	}
	var closeErr *websocket.CloseError
	if !errors.As(ev.Err, &closeErr) || closeErr.Code != websocket.CloseInternalServerErr {
		t.Errorf("Err event = %v, want it to keep the close code %d", ev.Err, websocket.CloseInternalServerErr)
	}
}

// TestAHandshakeRefusalKeepsNoCloseText fails if Open's error carries the
// text of the close frame Deepgram ended the handshake with: it may quote
// the Settings, whose prompt holds the room's words.
func TestAHandshakeRefusalKeepsNoCloseText(t *testing.T) {
	const words = "ROOM-WORDS-IN-SETTINGS"
	s := startStandIn(t)
	agent, err := New(&Config{APIKey: "k", URL: s.url})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := agent.Open(context.Background(), &voice.Exchange{Out: newFakeOutput(), Tools: tools(nil)})
		done <- err
	}()
	c := s.accept(t)
	c.expect(t, "Settings")
	c.dropWith(websocket.ClosePolicyViolation, words)
	err = <-done
	if err == nil || strings.Contains(err.Error(), words) {
		t.Fatalf("Open = %v, want an error without the close frame's text", err)
	}
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) || closeErr.Code != websocket.ClosePolicyViolation {
		t.Errorf("Open = %v, want it to keep the close code", err)
	}
}
