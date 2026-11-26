package gptlive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// seedText is the text of the session's starting history, which must be
// user messages of one input_text part each.
func seedText(t *testing.T, items []initialItem) string {
	t.Helper()
	var parts []string
	for _, item := range items {
		if item.Type != "message" || item.Role != "user" || len(item.Content) != 1 || item.Content[0].Type != "input_text" {
			t.Fatalf("history item %+v, want a user message with one input_text part", item)
		}
		parts = append(parts, item.Content[0].Text)
	}
	return strings.Join(parts, "\n")
}

func TestOpenStartsAClientDelegationSessionSeededWithTheMeeting(t *testing.T) {
	srv := newLiveServer(t)
	_, _, start := openSession(t, srv, testExchange(newFakeOutput(), newFakeTools(voice.AnswerResult{})))

	cfg := start.Session
	if cfg.Model != "gpt-live-1" {
		t.Errorf("model %q, want gpt-live-1", cfg.Model)
	}
	if cfg.Delegation.Type != "client" {
		t.Errorf("delegation %q, want client", cfg.Delegation.Type)
	}
	if cfg.Audio.Format != (audioFormat{Type: "audio/pcm", Rate: 24000}) {
		t.Errorf("audio format %+v, want audio/pcm at 24000 Hz", cfg.Audio.Format)
	}
	if !strings.Contains(string(start.raw), `"store":false`) {
		t.Errorf("session.start %s does not set store false, so OpenAI could keep the room's audio", start.raw)
	}
	for _, want := range []string{
		"Wispr",
		"only when someone",
		"Backchannel policy: Do not produce any backchannel",
		"Interruption policy:",
		"<|SpawnThinking|>",
		"exactly",
	} {
		if !strings.Contains(cfg.Instructions, want) {
			t.Errorf("instructions do not say %q:\n%s", want, cfg.Instructions)
		}
	}

	text := seedText(t, cfg.Input)
	first := strings.Index(text, "Speaker 0: Let's check on the recorder work.")
	second := strings.Index(text, "Speaker 1: Wispr knows.")
	asker := strings.Index(text, "what is happening with ABC-123")
	if first < 0 || second < first || asker < second {
		t.Errorf("history does not hold the labelled transcript, oldest first, then the asker's words:\n%s", text)
	}
}

func TestOpenSeedsAtMost8192TokensOfTheNewestHistory(t *testing.T) {
	x := testExchange(newFakeOutput(), newFakeTools(voice.AnswerResult{}))
	x.Recent = nil
	for i := range 3000 {
		x.Recent = append(x.Recent, voice.TurnText{Speaker: i % 3, Text: fmt.Sprintf("Line %04d is about the quarterly figures and the plan.", i)})
	}
	srv := newLiveServer(t)
	_, _, start := openSession(t, srv, x)

	text := seedText(t, start.Session.Input)
	// A token of English text is two bytes or more, so this many bytes is at
	// most 8,192 tokens.
	if len(text) > 8192*2 {
		t.Errorf("history is %d bytes, more than 8,192 tokens can hold", len(text))
	}
	if !strings.Contains(text, "Line 2999 is about") || strings.Contains(text, "Line 0000 is about") {
		t.Errorf("history does not keep the newest lines and drop the oldest")
	}
	if !strings.Contains(text, "what is happening with ABC-123") {
		t.Errorf("history dropped the asker's words")
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "Speaker") && !strings.HasSuffix(line, "the plan.") {
			t.Errorf("history cut a line: %q", line)
		}
	}
}

func TestOpenFailsWhenTheServerRefusesTheStart(t *testing.T) {
	srv := newLiveServer(t)
	v, err := New(Config{URL: srv.url(), Token: func(context.Context) (string, error) { return "access-token", nil }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		s, err := v.Open(context.Background(), testExchange(newFakeOutput(), newFakeTools(voice.AnswerResult{})))
		if s != nil {
			_ = s.Close()
		}
		done <- err
	}()
	c := srv.conn()
	c.expect(evSessionStart)
	c.send(map[string]any{"type": evError, "event_id": "event_err", "error": map[string]any{"type": "invalid_request_error", "code": "model_not_found", "message": "The project cannot use gpt-live-1."}})
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "model_not_found") {
			t.Fatalf("Open = %v, want the server's model_not_found", err)
		}
	case <-time.After(testWait):
		t.Fatal("Open did not return after the server refused the start")
	}
	c.closed()
}

func TestOpenFailsWithoutAnAccessToken(t *testing.T) {
	srv := newLiveServer(t)
	v, err := New(Config{URL: srv.url(), Token: func(context.Context) (string, error) { return "", errors.New("exchange refused: invalid_grant") }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s, err := v.Open(context.Background(), testExchange(newFakeOutput(), newFakeTools(voice.AnswerResult{}))); err == nil || !strings.Contains(err.Error(), "invalid_grant") {
		if s != nil {
			_ = s.Close()
		}
		t.Fatalf("Open = %v, want the token's failure", err)
	}

	bad, err := New(Config{URL: srv.url(), Token: func(context.Context) (string, error) { return "wrong-token", nil }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s, err := bad.Open(context.Background(), testExchange(newFakeOutput(), newFakeTools(voice.AnswerResult{}))); err == nil || !strings.Contains(err.Error(), "401") {
		if s != nil {
			_ = s.Close()
		}
		t.Fatalf("Open with a refused bearer = %v, want the HTTP 401", err)
	}
}

func TestTheTapStreamsToTheSessionAt24kHzFromTheWakeOn(t *testing.T) {
	x := testExchange(newFakeOutput(), newFakeTools(voice.AnswerResult{}))
	tap := make(chan audio.Frame, 4)
	x.Tap = tap
	// Audio from before session.started reaches the session once it starts.
	tap <- audio.Frame{PCM: ramp(0, 1600), Rate: 16000, Channels: 1}
	srv := newLiveServer(t)
	_, c, _ := openSession(t, srv, x)
	tap <- audio.Frame{PCM: ramp(1600, 1600), Rate: 16000, Channels: 1}

	var got []int16
	deadline := time.After(testWait)
	for len(got) < 4790 {
		select {
		case ev := <-c.in:
			if ev.Type == evInputAudioAppend {
				got = append(got, decodeAudio(t, ev.Audio)...)
			}
		case <-deadline:
			t.Fatalf("the session sent %d samples of 200 ms of tap audio, want 4,800 at 24 kHz", len(got))
		}
	}
	if len(got) > 4800 {
		t.Fatalf("the session sent %d samples for 200 ms at 24 kHz", len(got))
	}
	// A 16 kHz ramp read at 24 kHz climbs two thirds of a step each sample,
	// across the two frames with no jump between them.
	for k, s := range got {
		if want := float64(k) * 2 / 3; float64(s) < want-1 || float64(s) > want+1 {
			t.Fatalf("sample %d = %d, want %.1f: the tap audio is not 16 kHz read at 24 kHz", k, s, want)
		}
	}
}

func TestModelAudioPlaysOnTheVoiceInputAt24kHz(t *testing.T) {
	out := newFakeOutput()
	srv := newLiveServer(t)
	s, c, _ := openSession(t, srv, testExchange(out, newFakeTools(voice.AnswerResult{})))

	c.audioDelta(ramp(0, 2400))
	p := out.nextSpeech(t)
	waitEvent(t, s, voice.EventSpeechStarted)
	p.waitSamples(t, 2400)
	if got := p.samples(); fmt.Sprint(got) != fmt.Sprint(ramp(0, 2400)) {
		t.Fatalf("the voice input played other samples than the model sent")
	}
	p.mu.Lock()
	for i, f := range p.formats {
		if f != (audio.AudioFormat{SampleRate: 24000, Channels: 1, Encoding: audio.EncodingPCM16}) {
			t.Errorf("frame %d declared %+v, want mono PCM16 at 24 kHz", i, f)
		}
		if len(p.frames[i]) > 960 {
			t.Errorf("frame %d is %d bytes, more than 20 ms", i, len(p.frames[i]))
		}
	}
	p.mu.Unlock()
	waitEvent(t, s, voice.EventSpeechEnded)
	p.mu.Lock()
	drained := p.drained
	p.mu.Unlock()
	if !drained {
		t.Error("the speech ended before its playback drained")
	}
}

func TestADelegationRunsTheAnswerToolAndSpeaksItsResult(t *testing.T) {
	tools := newFakeTools(voice.AnswerResult{Say: "ABC-123 is in review, and PR 42 waits on CI.", Context: "The holder is the recorder agent, live."})
	srv := newLiveServer(t)
	s, c, _ := openSession(t, srv, testExchange(newFakeOutput(), tools))

	c.heard("Wispr, what's the status of ")
	c.heard("ABC-123?")
	c.delegate("item_1")

	ev := waitEvent(t, s, voice.EventToolCall)
	if ev.Call.ID != "item_1" || ev.Call.Name != voice.AnswerTool {
		t.Fatalf("tool call %+v, want %s for delegation item_1", ev.Call, voice.AnswerTool)
	}
	var args voice.AnswerArgs
	if err := json.Unmarshal(ev.Call.Args, &args); err != nil {
		t.Fatalf("tool call args %s: %v", ev.Call.Args, err)
	}
	if args.Question != "what is happening with ABC-123" {
		t.Errorf("question %q, want the room's asker", args.Question)
	}
	if !strings.Contains(args.Transcript, "Speaker 0: Let's check on the recorder work.") {
		t.Errorf("transcript %q does not hold the meeting before the wake", args.Transcript)
	}
	if run := tools.call(t); run.ID != ev.Call.ID || string(run.Args) != string(ev.Call.Args) {
		t.Errorf("the tool ran %+v, not the call the session reported", run)
	}

	thinking := c.next()
	commentary := c.next()
	if thinking.Type != evThinkingAppend || thinking.DelegationID == nil || *thinking.DelegationID != "item_1" || thinking.Content != "The holder is the recorder agent, live." {
		t.Errorf("first append %s, want the result's silent context on item_1", thinking.raw)
	}
	if commentary.Type != evCommentaryAppend || commentary.DelegationID == nil || *commentary.DelegationID != "item_1" || commentary.Content != "ABC-123 is in review, and PR 42 waits on CI." {
		t.Errorf("second append %s, want the result to say on item_1", commentary.raw)
	}
}

func TestDeliverSpeaksALateHolderAnswerOnItsDelegation(t *testing.T) {
	tools := newFakeTools(voice.AnswerResult{Say: "I asked the agent on ABC-123."})
	srv := newLiveServer(t)
	s, c, _ := openSession(t, srv, testExchange(newFakeOutput(), tools))
	c.delegate("item_7")
	tools.call(t)
	c.expect(evCommentaryAppend)

	late, _ := json.Marshal(voice.AnswerResult{Say: "The agent says PR 42 merged this morning."})
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	if err := s.Deliver(ctx, "item_7", late); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	ev := c.expect(evCommentaryAppend)
	if ev.DelegationID == nil || *ev.DelegationID != "item_7" || ev.Content != "The agent says PR 42 merged this morning." {
		t.Fatalf("late answer %s, want it said on item_7", ev.raw)
	}

	if err := s.Deliver(ctx, "item_unknown", late); !errors.Is(err, ErrUnknownCall) {
		t.Fatalf("Deliver to a delegation the session never had = %v, want ErrUnknownCall", err)
	}
}

func TestDeliverReportsTheServersRefusal(t *testing.T) {
	tools := newFakeTools(voice.AnswerResult{Say: "I asked the agent."})
	srv := newLiveServer(t)
	srv.autoAck = false
	s, c, _ := openSession(t, srv, testExchange(newFakeOutput(), tools))
	c.delegate("item_2")
	tools.call(t)
	c.expect(evCommentaryAppend) // the first answer, left unacknowledged

	late, _ := json.Marshal(voice.AnswerResult{Say: "The agent answered."})
	done := make(chan error, 1)
	go func() { done <- s.Deliver(context.Background(), "item_2", late) }()
	ev := c.expect(evCommentaryAppend)
	select {
	case err := <-done:
		t.Fatalf("Deliver returned %v before the server answered", err)
	case <-time.After(50 * time.Millisecond):
	}
	c.send(map[string]any{"type": evError, "event_id": "event_err", "error": map[string]any{"type": "invalid_request_error", "code": "fixture_active", "message": "Commentary is rejected while a fixture plays.", "client_event_id": ev.EventID}})
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "fixture_active") {
			t.Fatalf("Deliver = %v, want the server's fixture_active", err)
		}
	case <-time.After(testWait):
		t.Fatal("Deliver did not return after the server refused it")
	}
}

func TestSayReturnsOnceGPTLiveHasSpokenTheText(t *testing.T) {
	out := newFakeOutput()
	srv := newLiveServer(t)
	srv.autoAck = false
	s, c, _ := openSession(t, srv, testExchange(out, newFakeTools(voice.AnswerResult{})))

	done := make(chan error, 1)
	go func() {
		done <- s.Say(context.Background(), "The agent on ABC-123 answered: the fix is merged.", voice.Answer)
	}()
	ev := c.expect(evCommentaryAppend)
	if ev.DelegationID != nil || !strings.Contains(string(ev.raw), `"delegation_id":null`) {
		t.Errorf("Say sent %s, want session-wide commentary (delegation_id null)", ev.raw)
	}
	if ev.Content != "The agent on ABC-123 answered: the fix is merged." {
		t.Errorf("Say sent content %q", ev.Content)
	}
	c.send(map[string]any{"type": evCommentaryAppended, "event_id": "event_ack", "client_event_id": ev.EventID, "start_ms": 10, "end_ms": 20})
	// The acknowledgement says the text reached the model, not that the
	// room heard it: Say waits for the speech, so that an exchange that ends
	// once Say returns does not cut it.
	select {
	case err := <-done:
		t.Fatalf("Say returned %v before GPT-Live spoke", err)
	case <-time.After(100 * time.Millisecond):
	}
	c.audioDelta(ramp(1000, 2400))
	p := out.nextSpeech(t)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Say: %v", err)
		}
	case <-time.After(testWait):
		t.Fatal("Say did not return after GPT-Live spoke")
	}
	p.mu.Lock()
	drained := p.drained
	p.mu.Unlock()
	if !drained || len(p.samples()) != 2400 {
		t.Fatalf("Say returned before its speech played: drained %v, %d of 2400 samples", drained, len(p.samples()))
	}
}

func TestSayFailsWhenGPTLiveSaysNothing(t *testing.T) {
	defer func(d time.Duration) { sayStartTimeout = d }(sayStartTimeout)
	sayStartTimeout = 200 * time.Millisecond
	srv := newLiveServer(t)
	s, _, _ := openSession(t, srv, testExchange(newFakeOutput(), newFakeTools(voice.AnswerResult{})))

	if err := s.Say(context.Background(), "Say this.", voice.Answer); !errors.Is(err, ErrNoSpeech) {
		t.Fatalf("Say with no speech from GPT-Live = %v, want ErrNoSpeech", err)
	}
}

func TestLongCommentaryGoesInAppendsOf500TokensOrLess(t *testing.T) {
	srv := newLiveServer(t)
	s, c, _ := openSession(t, srv, testExchange(newFakeOutput(), newFakeTools(voice.AnswerResult{})))

	var sentences []string
	for i := range 120 {
		sentences = append(sentences, fmt.Sprintf("Sentence %d says the work on ABC-%d is still moving along.", i, i))
	}
	text := strings.Join(sentences, " ")
	done := make(chan error, 1)
	go func() { done <- s.Say(context.Background(), text, voice.Fact) }()

	var got []string
	for len(strings.Join(got, " ")) < len(text) {
		ev := c.expect(evCommentaryAppend)
		// A token is two bytes or more, so this many bytes is at most 500.
		if len(ev.Content) > 1000 {
			t.Errorf("one append is %d bytes, more than 500 tokens can hold", len(ev.Content))
		}
		if !strings.HasSuffix(ev.Content, ".") {
			t.Errorf("an append ends mid-sentence: %q", ev.Content[max(0, len(ev.Content)-40):])
		}
		got = append(got, ev.Content)
	}
	if strings.Join(got, " ") != text {
		t.Errorf("the appends do not add up to the text")
	}
	c.audioDelta(ramp(1000, 480))
	if err := returns(t, done, testWait, "Say"); err != nil {
		t.Fatalf("Say: %v", err)
	}
}

func TestSilenceFromTheModelOpensNoSpeech(t *testing.T) {
	out := newFakeOutput()
	srv := newLiveServer(t)
	_, c, _ := openSession(t, srv, testExchange(out, newFakeTools(voice.AnswerResult{})))

	// A second of silence opens no speech, so the speaker's stream stays shut.
	for range 10 {
		c.audioDelta(make([]int16, 2400))
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(speechGap)
	if n := len(out.speeches()); n != 0 {
		t.Fatalf("silence from the model began %d speeches", n)
	}

	// Speech plays, with the quiet inside it.
	c.audioDelta(ramp(1000, 2400))
	c.audioDelta(make([]int16, 480))
	c.audioDelta(ramp(5000, 480))
	p := out.nextSpeech(t)
	p.waitSamples(t, 2400+480+480)
	got := p.samples()
	if got[0] != 1000 || got[2400] != 0 || got[2880] != 5000 {
		t.Fatalf("speech played %d, %d, %d at the joins, want the speech, its quiet, and the rest", got[0], got[2400], got[2880])
	}

	// Silence after the speech has ended plays nothing.
	time.Sleep(speechGap + 100*time.Millisecond)
	c.audioDelta(make([]int16, 2400))
	time.Sleep(100 * time.Millisecond)
	if n := len(out.samples()); n != 2400+480+480 {
		t.Fatalf("silence after the speech played: %d samples, want %d", n, 2400+480+480)
	}
}

func TestTheVoiceDrivesItsOwnTurn(t *testing.T) {
	v, err := New(Config{Token: func(context.Context) (string, error) { return "t", nil }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var d voice.TurnDriver = v
	if !d.DrivesTurn() {
		t.Fatal("voice B does not drive its own turn, so the Supervisor would hand the turn to the Concierge too")
	}
}

func TestInterruptStopsTheModelsSpeechAndTellsItToStop(t *testing.T) {
	out := newFakeOutput()
	srv := newLiveServer(t)
	s, c, _ := openSession(t, srv, testExchange(out, newFakeTools(voice.AnswerResult{})))

	c.audioDelta(ramp(0, 2400))
	p := out.nextSpeech(t)
	p.waitSamples(t, 2400)

	s.Interrupt()
	p.cancel() // the Supervisor cancels the voice input
	stop := c.expect(evInstructionsAppend)
	if stop.DelegationID != nil || !strings.Contains(stop.Content, "Stop speaking") {
		t.Errorf("Interrupt sent %s, want a session-wide instruction to stop speaking", stop.raw)
	}
	waitEvent(t, s, voice.EventSpeechEnded)

	// The rest of the interrupted speech never plays.
	c.audioDelta(ramp(10000, 2400))
	time.Sleep(100 * time.Millisecond)
	for _, v := range out.samples() {
		if v >= 10000 {
			t.Fatal("audio of the interrupted speech played after Interrupt")
		}
	}

	// A new speech, after a pause in the model's audio, plays.
	time.Sleep(speechGap + 100*time.Millisecond)
	c.audioDelta(ramp(20000, 480))
	next := out.nextSpeech(t)
	next.waitSamples(t, 480)
	if next.samples()[0] != 20000 {
		t.Fatalf("the next speech played %d first, want its own audio", next.samples()[0])
	}
}

func TestNoModelAudioPlaysOnceTheSessionCloses(t *testing.T) {
	out := newFakeOutput()
	srv := newLiveServer(t)
	s, c, _ := openSession(t, srv, testExchange(out, newFakeTools(voice.AnswerResult{})))

	served := make(chan struct{})
	go func() {
		defer close(served)
		for ev := range c.in {
			if ev.Type == evSessionClose {
				break
			}
		}
		// Trailing output, as the server sends it up to session.closed.
		c.audioDelta(ramp(30000, 2400))
		c.send(map[string]any{"type": evSessionClosed, "event_id": "event_closed", "reason": "close_requested", "usage": map[string]any{"seconds": 3}})
	}()
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	<-served
	time.Sleep(50 * time.Millisecond)
	if n := len(out.samples()); n != 0 {
		t.Fatalf("%d samples of model audio played after Close", n)
	}
}

func TestAServerErrorReachesEvents(t *testing.T) {
	srv := newLiveServer(t)
	s, c, _ := openSession(t, srv, testExchange(newFakeOutput(), newFakeTools(voice.AnswerResult{})))

	c.send(map[string]any{"type": evError, "event_id": "event_err", "error": map[string]any{"type": "server_error", "code": "internal_error", "message": "The Live engine failed."}})
	ev := waitEvent(t, s, voice.EventErr)
	if ev.Err == nil || !strings.Contains(ev.Err.Error(), "internal_error") {
		t.Fatalf("error event %v, want the server's internal_error", ev.Err)
	}
}

func TestADisconnectReachesEventsAndEndsTheSession(t *testing.T) {
	srv := newLiveServer(t)
	s, c, _ := openSession(t, srv, testExchange(newFakeOutput(), newFakeTools(voice.AnswerResult{})))

	_ = c.ws.Close()
	ev := waitEvent(t, s, voice.EventErr)
	if !errors.Is(ev.Err, ErrDisconnected) {
		t.Fatalf("error event %v, want ErrDisconnected", ev.Err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	if err := s.Say(ctx, "Anyone there?", voice.Ack); err == nil {
		t.Fatal("Say on a disconnected session succeeded")
	}
}

func TestASessionTheServerEndsReachesEvents(t *testing.T) {
	srv := newLiveServer(t)
	s, c, _ := openSession(t, srv, testExchange(newFakeOutput(), newFakeTools(voice.AnswerResult{})))

	c.send(map[string]any{"type": evSessionClosed, "event_id": "event_closed", "reason": "expired", "usage": map[string]any{"seconds": 1800}})
	ev := waitEvent(t, s, voice.EventErr)
	if !errors.Is(ev.Err, ErrDisconnected) || strings.Contains(ev.Err.Error(), "expired") {
		t.Fatalf("error event %v, want ErrDisconnected without the server's reason", ev.Err)
	}
}

func TestCloseTearsDownWithin100ms(t *testing.T) {
	tools := newFakeTools(voice.AnswerResult{})
	tools.block = true
	x := testExchange(newFakeOutput(), tools)
	x.Tap = make(chan audio.Frame)
	srv := newLiveServer(t)
	s, c, _ := openSession(t, srv, x)
	c.delegate("item_3")
	tools.call(t) // a tool run is under way, and the server never answers session.close

	begin := time.Now()
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if took := time.Since(begin); took > 100*time.Millisecond {
		t.Fatalf("Close took %v, want 100 ms at most", took)
	}
	c.expect(evSessionClose)
	c.closed()
	for {
		select {
		case _, ok := <-s.Events():
			if !ok {
				return
			}
		case <-time.After(testWait):
			t.Fatal("Events did not close after Close")
		}
	}
}

func TestTheVoicesWordsAndWhatItHeardReachEvents(t *testing.T) {
	srv := newLiveServer(t)
	s, c, _ := openSession(t, srv, testExchange(newFakeOutput(), newFakeTools(voice.AnswerResult{})))

	c.heard("Wispr, how is ")
	c.heard("ABC-123 doing?")
	c.speak("It is in review.")
	if ev := waitEvent(t, s, voice.EventUserTurn); ev.Text != "Wispr, how is ABC-123 doing?" {
		t.Errorf("user turn %q, want what the model heard before it spoke", ev.Text)
	}
	if ev := waitEvent(t, s, voice.EventAgentText); ev.Text != "It is in review." {
		t.Errorf("agent text %q, want the model's words once they played", ev.Text)
	}
}

// The Supervisor gives Open the exchange's context, which its 2-minute limit
// and every ending cancel: the session, and the room audio it streams to
// OpenAI, must end with it.
func TestTheSessionEndsWithItsExchangesContext(t *testing.T) {
	x := testExchange(newFakeOutput(), newFakeTools(voice.AnswerResult{}))
	tap := make(chan audio.Frame, 4)
	x.Tap = tap
	ctx, cancel := context.WithCancel(context.Background())
	srv := newLiveServer(t)
	s, c, _ := openSessionCtx(t, srv, ctx, x)
	tap <- audio.Frame{PCM: ramp(0, 1600), Rate: 16000, Channels: 1}
	c.expect(evInputAudioAppend)

	cancel()
	c.expect(evSessionClose)
	c.closed()
	waitClosed(t, s.Events())
}

// The Supervisor closes the tap's stream the moment an exchange ends, before
// it cancels the exchange. Room audio held while the session started must
// then never reach OpenAI, though the light went off after it was heard.
func TestTapAudioHeldWhileStartingIsDroppedWhenTheExchangeEnds(t *testing.T) {
	x := testExchange(newFakeOutput(), newFakeTools(voice.AnswerResult{}))
	tap := make(chan audio.Frame, 4)
	x.Tap = tap
	tap <- audio.Frame{PCM: ramp(0, 1600), Rate: 16000, Channels: 1}
	close(tap)
	srv := newLiveServer(t)
	v := newTestVoice(t, srv)
	opened := make(chan voice.Session, 1)
	go func() {
		s, err := v.Open(context.Background(), x)
		if err != nil {
			t.Errorf("Open: %v", err)
		}
		opened <- s
	}()
	c := srv.conn()
	time.Sleep(100 * time.Millisecond) // the session reads the frame and the close while it starts
	c.start()
	s := <-opened
	if s != nil {
		defer func() { _ = s.Close() }()
	}
	c.noAudio(300 * time.Millisecond)
}

// GPT-Live can delegate before its transcript of the question arrives: the
// docs say transcript delivery can be uneven. The question must still carry
// the asker's words.
func TestADelegationBeforeTheRoomsFinalWaitsForIt(t *testing.T) {
	tools := newFakeTools(voice.AnswerResult{Say: "It is in review."})
	x := testExchange(newFakeOutput(), tools)
	rs := withRoom(x, "")
	srv := newLiveServer(t)
	s, c, _ := openSession(t, srv, x)

	c.delegate("item_q")
	time.Sleep(100 * time.Millisecond)
	rs.set(voice.RoomView{Live: true, Asker: "Wispr, what is the status of ABC-123?", Revision: 1})
	c.heard("Wispr, what is the status of ABC-123?")
	ev := waitEvent(t, s, voice.EventToolCall)
	var args voice.AnswerArgs
	if err := json.Unmarshal(ev.Call.Args, &args); err != nil {
		t.Fatalf("args %s: %v", ev.Call.Args, err)
	}
	if args.Question != "Wispr, what is the status of ABC-123?" {
		t.Fatalf("question %q, want the room's asker, awaited", args.Question)
	}
}

// With a live room that gives no asker within the bound, the delegation
// completes silently: there is no history fallback (Main, after Sol's
// eighteenth review of #191).
func TestADelegationWithNoRoomAskerCompletesSilently(t *testing.T) {
	tools := newFakeTools(voice.AnswerResult{Say: "It is in review."})
	x := testExchange(newFakeOutput(), tools)
	withRoom(x, "")
	srv := newLiveServer(t)
	_, c, _ := openSession(t, srv, x)

	c.delegate("item_n")
	if first := c.expect(evThinkingAppend); first.DelegationID == nil || *first.DelegationID != "item_n" || first.Content != "" {
		t.Fatalf("silent completion %+v, want an empty thinking append for item_n", first)
	}
	select {
	case call := <-tools.calls:
		t.Fatalf("a delegation with no room asker ran the tool: %s", call.ID)
	case <-time.After(100 * time.Millisecond):
	}
}

// The question is the room's asker (testExchange's AskerTurn), not GPT-Live's
// transcript of the same words (Main, after Sol's eighteenth review of #191).
func TestTheQuestionIsTheRoomsAskerNotTheHeardCopy(t *testing.T) {
	tools := newFakeTools(voice.AnswerResult{Say: "It is in review."})
	srv := newLiveServer(t)
	s, c, _ := openSession(t, srv, testExchange(newFakeOutput(), tools))

	c.heard("What is happening with ABC-123?")
	c.delegate("item_d")
	ev := waitEvent(t, s, voice.EventToolCall)
	var args voice.AnswerArgs
	if err := json.Unmarshal(ev.Call.Args, &args); err != nil {
		t.Fatalf("args %s: %v", ev.Call.Args, err)
	}
	if args.Question != "what is happening with ABC-123" {
		t.Fatalf("question %q, want the room's asker", args.Question)
	}
}

// After Interrupt, GPT-Live's transcript of the speech that no longer plays
// is not Wispr's speech: the Supervisor would take a person's matching words
// for Wispr's echo.
func TestTheWordsOfAnInterruptedSpeechAreNotWisprsSpeech(t *testing.T) {
	out := newFakeOutput()
	srv := newLiveServer(t)
	s, c, _ := openSession(t, srv, testExchange(out, newFakeTools(voice.AnswerResult{})))

	at := c.inputMs()
	c.audioDelta(ramp(1000, 2400)) // 100 ms, which plays
	out.nextSpeech(t).waitSamples(t, 2400)
	s.Interrupt()
	waitEvent(t, s, voice.EventSpeechEnded)
	c.saidAt("words nobody heard", at+150) // in the rest, which does not play
	c.audioDelta(ramp(1000, 2400))
	deadline := time.After(200 * time.Millisecond)
	for done := false; !done; {
		select {
		case ev := <-s.Events():
			if ev.Kind == voice.EventAgentText {
				t.Fatalf("agent text %q from a speech Interrupt dropped", ev.Text)
			}
		case <-deadline:
			done = true
		}
	}

	// The next speech, after a pause, is Wispr's again.
	time.Sleep(speechGap)
	c.speak("Sure.")
	if ev := waitEvent(t, s, voice.EventAgentText); ev.Text != "Sure." {
		t.Fatalf("agent text %q, want the next speech's words", ev.Text)
	}
}

// A Supervisor that reads Events slowly must not stall the session: a long
// answer of many transcript pieces plays to its end.
func TestAnUnreadEventsChannelDoesNotStallTheSpeech(t *testing.T) {
	out := newFakeOutput()
	srv := newLiveServer(t)
	_, c, _ := openSession(t, srv, testExchange(out, newFakeTools(voice.AnswerResult{})))

	for i := range 300 {
		c.said(fmt.Sprintf("word%d ", i))
		c.audioDelta(ramp(1000, 240))
	}
	p := out.nextSpeech(t)
	p.waitSamples(t, 300*240)
}

func TestOpenReportsWhyTheServerRefusedTheConnection(t *testing.T) {
	srv := newLiveServer(t)
	srv.refuseStatus = http.StatusForbidden
	srv.refuseBody = `{"error":{"message":"The project cannot use gpt-live-1.","type":"invalid_request_error","code":"model_not_found"}}`
	_, err := newTestVoice(t, srv).Open(context.Background(), testExchange(newFakeOutput(), newFakeTools(voice.AnswerResult{})))
	if err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "model_not_found") {
		t.Fatalf("Open = %v, want the HTTP 403 and the API's model_not_found", err)
	}
}

// A delegation the Concierge has nothing to say about still gets an answer:
// GPT-Live is told not to guess while it waits.
func TestAnEmptyAnswerStillAnswersTheDelegation(t *testing.T) {
	tools := newFakeTools(voice.AnswerResult{})
	srv := newLiveServer(t)
	_, c, _ := openSession(t, srv, testExchange(newFakeOutput(), tools))

	c.delegate("item_e")
	tools.call(t)
	ev := c.expect(evCommentaryAppend)
	if ev.DelegationID == nil || *ev.DelegationID != "item_e" || !strings.Contains(ev.Content, "nothing") {
		t.Fatalf("append %s, want a commentary on item_e saying nothing was found", ev.raw)
	}
}

// Wispr says a held answer in a session nobody addressed: the instructions
// must have GPT-Live say the application's commentary.
func TestTheInstructionsSayApplicationCommentaryAtOnce(t *testing.T) {
	if !strings.Contains(instructions, "say it at once") {
		t.Fatalf("instructions do not tell GPT-Live to say the application's commentary at once:\n%s", instructions)
	}
}

// An API error's message can echo what the session sent, and the error goes
// to the journal: its text is the code, type and parameter only.
func TestAnAPIErrorsTextLeavesOutItsMessage(t *testing.T) {
	e := &APIError{Type: "invalid_request_error", Code: "bad_content", Message: "the words someone said", Param: "content"}
	if got := e.Error(); strings.Contains(got, "said") || !strings.Contains(got, "bad_content") || !strings.Contains(got, "content") {
		t.Fatalf("Error() = %q, want the code and parameter and no message", got)
	}
}

// A code or type that is free text, not a plain token, is left out of the
// error's text.
func TestAnAPIErrorsCodeIsAPlainToken(t *testing.T) {
	e := &APIError{Type: "the words someone said", Code: "bad content someone said"}
	if got := e.Error(); strings.Contains(got, "said") || got != "gpt-live error server_error" {
		t.Fatalf("Error() = %q, want the fallback code and no free text", got)
	}
}

func TestSayAndDeliverAfterCloseReportClosed(t *testing.T) {
	tools := newFakeTools(voice.AnswerResult{Say: "x"})
	srv := newLiveServer(t)
	s, c, _ := openSession(t, srv, testExchange(newFakeOutput(), tools))
	c.delegate("item_c")
	tools.call(t)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	result, _ := json.Marshal(voice.AnswerResult{Say: "late"})
	if err := s.Say(context.Background(), "hello", voice.Ack); !errors.Is(err, ErrClosed) {
		t.Errorf("Say after Close = %v, want ErrClosed", err)
	}
	if err := s.Deliver(context.Background(), "item_c", result); !errors.Is(err, ErrClosed) {
		t.Errorf("Deliver after Close = %v, want ErrClosed", err)
	}
}

// Once the session closes, audio that was already on its way plays nothing.
func TestTheClosedPlayerTakesNoAudio(t *testing.T) {
	p := newPlayer(&session{})
	p.stop()
	if p.push(ramp(1000, 480), time.Now()) || len(p.queue) != 0 {
		t.Fatal("a stopped player queued model audio")
	}
}
