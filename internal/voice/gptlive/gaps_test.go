package gptlive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// The tests in this file pin behaviours that single changes to the code
// broke with every other test still passing.

func answerArgs(t *testing.T, raw json.RawMessage) voice.AnswerArgs {
	t.Helper()
	var args voice.AnswerArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		t.Fatalf("tool call args %s: %v", raw, err)
	}
	return args
}

// returns waits for done to give an error within d.
func returns(t *testing.T, done <-chan error, d time.Duration, what string) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(d):
		t.Fatalf("%s did not return within %v", what, d)
		return nil
	}
}

// The Supervisor ends the exchange when Say returns: a second sentence after
// a short pause must still play.
func TestSayWaitsForASecondSentence(t *testing.T) {
	out := newFakeOutput()
	srv := newLiveServer(t)
	s, c, _ := openSession(t, srv, testExchange(out, newFakeTools(voice.AnswerResult{})))

	done := make(chan error, 1)
	go func() { done <- s.Say(context.Background(), "Say this.", voice.Answer) }()
	c.expect(evCommentaryAppend)
	c.audioDelta(ramp(1000, 2400))
	time.Sleep(speechGap + 300*time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("Say returned %v in the pause between two sentences", err)
	default:
	}
	c.audioDelta(ramp(5000, 2400))
	if err := returns(t, done, testWait, "Say"); err != nil {
		t.Fatalf("Say: %v", err)
	}
	if n := len(out.samples()); n != 4800 {
		t.Fatalf("Say returned with %d of 4,800 samples played", n)
	}
}

func TestSayReportsADisconnectWhileItWaitsForSpeech(t *testing.T) {
	srv := newLiveServer(t)
	s, c, _ := openSession(t, srv, testExchange(newFakeOutput(), newFakeTools(voice.AnswerResult{})))

	done := make(chan error, 1)
	go func() { done <- s.Say(context.Background(), "Say this.", voice.Answer) }()
	c.expect(evCommentaryAppend)
	time.Sleep(50 * time.Millisecond)
	_ = c.ws.Close()
	if err := returns(t, done, time.Second, "Say"); !errors.Is(err, ErrDisconnected) {
		t.Fatalf("Say on a connection that dropped = %v, want ErrDisconnected", err)
	}
}

func TestSayStopsWaitingForItsAcknowledgementWhenItsContextEnds(t *testing.T) {
	srv := newLiveServer(t)
	srv.autoAck = false
	s, c, _ := openSession(t, srv, testExchange(newFakeOutput(), newFakeTools(voice.AnswerResult{})))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Say(ctx, "Say this.", voice.Answer) }()
	c.expect(evCommentaryAppend)
	cancel()
	if err := returns(t, done, 500*time.Millisecond, "Say"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Say with its context cancelled = %v, want context.Canceled", err)
	}
}

func TestSayStopsWaitingForSpeechWhenItsContextEnds(t *testing.T) {
	srv := newLiveServer(t)
	s, c, _ := openSession(t, srv, testExchange(newFakeOutput(), newFakeTools(voice.AnswerResult{})))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Say(ctx, "Say this.", voice.Answer) }()
	c.expect(evCommentaryAppend)
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := returns(t, done, 500*time.Millisecond, "Say"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Say with its context cancelled = %v, want context.Canceled", err)
	}
}

func TestClosingWithAnAppendPendingReportsClosed(t *testing.T) {
	srv := newLiveServer(t)
	srv.autoAck = false
	s, c, _ := openSession(t, srv, testExchange(newFakeOutput(), newFakeTools(voice.AnswerResult{})))

	done := make(chan error, 1)
	go func() { done <- s.Say(context.Background(), "Say this.", voice.Answer) }()
	c.expect(evCommentaryAppend)
	_ = s.Close()
	if err := returns(t, done, 500*time.Millisecond, "Say"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Say whose acknowledgement was pending at Close = %v, want ErrClosed", err)
	}
}

// An error event can name its command at the top level rather than inside
// its error object.
func TestAnErrorNamingItsCommandAtTheTopLevelReachesIt(t *testing.T) {
	srv := newLiveServer(t)
	srv.autoAck = false
	s, c, _ := openSession(t, srv, testExchange(newFakeOutput(), newFakeTools(voice.AnswerResult{})))

	done := make(chan error, 1)
	go func() { done <- s.Say(context.Background(), "Say this.", voice.Answer) }()
	ev := c.expect(evCommentaryAppend)
	c.send(map[string]any{"type": evError, "event_id": "event_err", "client_event_id": ev.EventID, "error": map[string]any{"type": "invalid_request_error", "code": "rejected", "message": "No."}})
	if err := returns(t, done, time.Second, "Say"); err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("Say refused by an error naming it at the top level = %v, want the error", err)
	}
}

func TestADelegationForAnotherTargetRunsNoTool(t *testing.T) {
	tools := newFakeTools(voice.AnswerResult{Say: "x"})
	srv := newLiveServer(t)
	s, c, _ := openSession(t, srv, testExchange(newFakeOutput(), tools))

	c.heard("What is ABC-1?")
	c.send(map[string]any{"type": evDelegationCreated, "event_id": "event_r", "offset_ms": 1, "delegation": map[string]any{"id": "resp_1", "type": "delegation", "target": "responses", "response_id": "resp_x"}})
	select {
	case got := <-tools.calls:
		t.Fatalf("a delegation for responses ran the answer tool: %+v", got)
	case <-time.After(transcriptSettle + 400*time.Millisecond):
	}
	result, _ := json.Marshal(voice.AnswerResult{Say: "late"})
	if err := s.Deliver(context.Background(), "resp_1", result); !errors.Is(err, ErrUnknownCall) {
		t.Fatalf("Deliver on a delegation not for the client = %v, want ErrUnknownCall", err)
	}
}

// When the answer tool fails, the Supervisor speaks the failure itself (its
// own sorry, and the error sound): the voice must not say a second one. The
// delegation still gets silent context, so GPT-Live does not guess.
func TestAFailedAnswerToolClosesItsDelegationSilently(t *testing.T) {
	tools := newFakeTools(voice.AnswerResult{})
	tools.err = errors.New("search failed")
	srv := newLiveServer(t)
	_, c, _ := openSession(t, srv, testExchange(newFakeOutput(), tools))

	c.delegate("item_f")
	ev := c.next()
	if ev.Type != evThinkingAppend || ev.DelegationID == nil || *ev.DelegationID != "item_f" || ev.Content != lookupFailed {
		t.Fatalf("append %s, want silent context on item_f that the lookup failed", ev.raw)
	}
	select {
	case more := <-c.in:
		if more.Type == evCommentaryAppend {
			t.Fatalf("the session spoke a second apology: %s", more.raw)
		}
	case <-time.After(300 * time.Millisecond):
	}
}

func TestADelegationReportsWhatWasHeardAsTheUserTurn(t *testing.T) {
	srv := newLiveServer(t)
	s, c, _ := openSession(t, srv, testExchange(newFakeOutput(), newFakeTools(voice.AnswerResult{Say: "x"})))

	c.heard("Wispr, what about ABC-9?")
	c.delegate("item_u")
	if ev := waitEvent(t, s, voice.EventUserTurn); ev.Text != "Wispr, what about ABC-9?" {
		t.Fatalf("user turn %q, want what GPT-Live heard before it delegated", ev.Text)
	}
}

func TestASecondDelegationAsksOnlyWhatWasHeardSince(t *testing.T) {
	srv := newLiveServer(t)
	s, c, _ := openSession(t, srv, testExchange(newFakeOutput(), newFakeTools(voice.AnswerResult{Say: "x"})))

	c.heard("Tell me about ABC-7.")
	c.delegate("item_1")
	if q := answerArgs(t, waitEvent(t, s, voice.EventToolCall).Call.Args).Question; !strings.Contains(q, "what is happening with ABC-123") {
		t.Fatalf("first question %q lacks the asker's words at the wake", q)
	}
	c.heard("And ABC-8?")
	c.delegate("item_2")
	if q := answerArgs(t, waitEvent(t, s, voice.EventToolCall).Call.Args).Question; q != "And ABC-8?" {
		t.Fatalf("second question %q, want only what was heard since the first", q)
	}
}

// A second delegation straight after the first, with nothing new heard,
// asks the first question again.
func TestASecondDelegationWithNothingNewRepeatsTheFirstQuestion(t *testing.T) {
	x := testExchange(newFakeOutput(), newFakeTools(voice.AnswerResult{Say: "x"}))
	x.AskerTurn = ""
	srv := newLiveServer(t)
	s, c, _ := openSession(t, srv, x)

	c.heard("Is ABC-5 done?")
	c.delegate("item_1")
	waitEvent(t, s, voice.EventToolCall)
	c.delegate("item_2")
	if q := answerArgs(t, waitEvent(t, s, voice.EventToolCall).Call.Args).Question; q != "Is ABC-5 done?" {
		t.Fatalf("question %q, want the first question again", q)
	}
}

// The tap's audio held while the session starts keeps its first ten seconds:
// they hold the asker's first words.
func TestTheAudioHeldWhileStartingKeepsItsFirstTenSeconds(t *testing.T) {
	x := testExchange(newFakeOutput(), newFakeTools(voice.AnswerResult{}))
	tap := make(chan audio.Frame, 64)
	x.Tap = tap
	for i := range 44 { // 11 s
		pcm := make([]int16, 4000)
		for j := range pcm {
			pcm[j] = int16(100 * (i + 1))
		}
		tap <- audio.Frame{PCM: pcm, Rate: 16000, Channels: 1}
	}
	srv := newLiveServer(t)
	v := newTestVoice(t, srv)
	opened := make(chan voice.Session, 1)
	go func() {
		s, _ := v.Open(context.Background(), x)
		opened <- s
	}()
	c := srv.conn()
	time.Sleep(200 * time.Millisecond)
	c.start()
	if s := <-opened; s != nil {
		defer func() { _ = s.Close() }()
	}
	var got []int16
	for quiet := time.After(500 * time.Millisecond); ; {
		select {
		case ev := <-c.in:
			if ev.Type == evInputAudioAppend {
				got = append(got, decodeAudio(t, ev.Audio)...)
				quiet = time.After(500 * time.Millisecond)
			}
			continue
		case <-quiet:
		}
		break
	}
	// 40 frames of 6,000 samples fit, less the one sample the resampler holds
	// back for the next frame.
	if len(got) < earlyAudio-1 || len(got) > earlyAudio || got[0] != 100 || got[len(got)-1] != 4000 {
		t.Fatalf("sent %d samples from %d to %d, want the first %d (10 s), 100 to 4000", len(got), got[0], got[len(got)-1], earlyAudio)
	}
}

func TestThePlayerKeepsTheNewestTwoMinutes(t *testing.T) {
	p := newPlayer(&session{})
	old := make([]int16, maxQueuedAudio)
	for i := range old {
		old[i] = 1000
	}
	p.push(old, time.Now())
	p.push(ramp(2000, 480), time.Now())
	if len(p.queue) != maxQueuedAudio || p.queue[len(p.queue)-1] != 2479 {
		t.Fatalf("queue holds %d samples ending %d, want %d ending with the newest", len(p.queue), p.queue[len(p.queue)-1], maxQueuedAudio)
	}
}

func TestCloseGivesTheServerTimeToFinalize(t *testing.T) {
	srv := newLiveServer(t)
	s, c, _ := openSession(t, srv, testExchange(newFakeOutput(), newFakeTools(voice.AnswerResult{})))

	go func() {
		for ev := range c.in {
			if ev.Type == evSessionClose {
				time.Sleep(20 * time.Millisecond)
				c.send(map[string]any{"type": evSessionClosed, "event_id": "event_closed", "reason": "close_requested", "usage": map[string]any{"seconds": 1}})
				return
			}
		}
	}()
	begin := time.Now()
	_ = s.Close()
	if took := time.Since(begin); took < 15*time.Millisecond || took > 100*time.Millisecond {
		t.Fatalf("Close took %v with a server that finalizes in 20 ms, want it to wait for session.closed and stay under 100 ms", took)
	}
}

func TestCloseCancelsAToolRunUnderWay(t *testing.T) {
	tools := newFakeTools(voice.AnswerResult{})
	tools.block = true
	srv := newLiveServer(t)
	s, c, _ := openSession(t, srv, testExchange(newFakeOutput(), tools))
	c.delegate("item_b")
	tools.call(t)
	_ = s.Close()
	select {
	case <-tools.stopped:
	case <-time.After(time.Second):
		t.Fatal("the tool run went on after Close")
	}
}

func TestDeliverSendsALongAnswerInAppendsOf500TokensOrLess(t *testing.T) {
	tools := newFakeTools(voice.AnswerResult{Say: "I asked."})
	srv := newLiveServer(t)
	s, c, _ := openSession(t, srv, testExchange(newFakeOutput(), tools))
	c.delegate("item_l")
	tools.call(t)
	c.expect(evCommentaryAppend)

	var say, facts []string
	for i := range 60 {
		say = append(say, fmt.Sprintf("Spoken sentence %d is about ABC-%d and its review.", i, i))
		facts = append(facts, fmt.Sprintf("Silent fact %d says ABC-%d waits on CI.", i, i))
	}
	result, _ := json.Marshal(voice.AnswerResult{Say: strings.Join(say, " "), Context: strings.Join(facts, " ")})
	if err := s.Deliver(context.Background(), "item_l", result); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	var thinking, commentary []string
	for len(strings.Join(commentary, " ")) < len(strings.Join(say, " ")) {
		ev := c.next()
		if len(ev.Content) > appendLimit {
			t.Fatalf("one %s is %d bytes, more than 500 tokens can hold", ev.Type, len(ev.Content))
		}
		switch ev.Type {
		case evThinkingAppend:
			if len(commentary) > 0 {
				t.Fatal("silent context went after the commentary")
			}
			thinking = append(thinking, ev.Content)
		case evCommentaryAppend:
			commentary = append(commentary, ev.Content)
		}
	}
	if strings.Join(thinking, " ") != strings.Join(facts, " ") || strings.Join(commentary, " ") != strings.Join(say, " ") {
		t.Fatal("the appends do not add up to the answer")
	}
}

// The Supervisor cancels the voice input without an Interrupt when it stops
// the voice: the rest of that speech must not start a speech of its own.
func TestACancelledVoiceInputDropsTheRestOfItsSpeech(t *testing.T) {
	out := newFakeOutput()
	srv := newLiveServer(t)
	_, c, _ := openSession(t, srv, testExchange(out, newFakeTools(voice.AnswerResult{})))

	c.audioDelta(ramp(1000, 2400))
	p := out.nextSpeech(t)
	p.waitSamples(t, 2400)
	p.cancel()
	c.audioDelta(ramp(5000, 2400))
	time.Sleep(50 * time.Millisecond)
	c.audioDelta(ramp(9000, 2400))
	time.Sleep(200 * time.Millisecond)
	if n := len(out.speeches()); n != 1 {
		t.Fatalf("the rest of a cancelled speech began %d more speeches", n-1)
	}
}

func TestNearSilenceOpensNoSpeech(t *testing.T) {
	out := newFakeOutput()
	srv := newLiveServer(t)
	_, c, _ := openSession(t, srv, testExchange(out, newFakeTools(voice.AnswerResult{})))

	hum := make([]int16, 2400)
	for i := range hum {
		hum[i] = 100 - 200*int16(i%2)
	}
	c.audioDelta(hum)
	time.Sleep(200 * time.Millisecond)
	if n := len(out.speeches()); n != 0 {
		t.Fatalf("audio within ±100 began %d speeches", n)
	}
	for i := range hum {
		hum[i] = 200 - 400*int16(i%2)
	}
	c.audioDelta(hum)
	out.nextSpeech(t)
}

// Close stops the speech before it asks the server to finalize, not after.
func TestCloseStopsTheSpeechAtOnce(t *testing.T) {
	out := newFakeOutput()
	out.pace = true
	srv := newLiveServer(t)
	s, c, _ := openSession(t, srv, testExchange(out, newFakeTools(voice.AnswerResult{})))

	c.audioDelta(ramp(1000, SampleRate))
	p := out.nextSpeech(t)
	time.Sleep(100 * time.Millisecond)
	before := len(p.samples())
	_ = s.Close() // the server never answers session.close
	if after := len(p.samples()); after-before > writeFrame {
		t.Fatalf("%d samples played while Close waited for the server, want at most the frame under way", after-before)
	}
}

func TestInterruptEndsTheSpeechAtOnce(t *testing.T) {
	out := newFakeOutput()
	srv := newLiveServer(t)
	s, c, _ := openSession(t, srv, testExchange(out, newFakeTools(voice.AnswerResult{})))

	c.audioDelta(ramp(1000, 2400))
	p := out.nextSpeech(t)
	p.waitSamples(t, 2400)
	begin := time.Now()
	s.Interrupt()
	waitEvent(t, s, voice.EventSpeechEnded)
	if took := time.Since(begin); took > 100*time.Millisecond {
		t.Fatalf("the speech ended %v after Interrupt, want at once", took)
	}
	p.mu.Lock()
	drained := p.drained
	p.mu.Unlock()
	if drained {
		t.Fatal("Interrupt waited for the speech to drain")
	}
}

func TestOpenFailsAtOnceWhenTheConnectionDropsBeforeTheStart(t *testing.T) {
	srv := newLiveServer(t)
	done := make(chan error, 1)
	go func() {
		s, err := newTestVoice(t, srv).Open(context.Background(), testExchange(newFakeOutput(), newFakeTools(voice.AnswerResult{})))
		if s != nil {
			_ = s.Close()
		}
		done <- err
	}()
	c := srv.conn()
	c.expect(evSessionStart)
	_ = c.ws.Close()
	if err := returns(t, done, time.Second, "Open"); !errors.Is(err, ErrDisconnected) {
		t.Fatalf("Open on a connection that dropped before the start = %v, want ErrDisconnected", err)
	}
}

func TestOpenFailsAtOnceWhenTheSessionEndsBeforeTheStart(t *testing.T) {
	srv := newLiveServer(t)
	done := make(chan error, 1)
	go func() {
		s, err := newTestVoice(t, srv).Open(context.Background(), testExchange(newFakeOutput(), newFakeTools(voice.AnswerResult{})))
		if s != nil {
			_ = s.Close()
		}
		done <- err
	}()
	c := srv.conn()
	c.expect(evSessionStart)
	c.send(map[string]any{"type": evSessionClosed, "event_id": "event_closed", "reason": "expired", "usage": map[string]any{"seconds": 0}})
	if err := returns(t, done, time.Second, "Open"); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("Open on a session that ended before the start = %v, want its reason", err)
	}
}

func TestOpenEndsWhenItsContextEndsBeforeTheSessionStarts(t *testing.T) {
	x := testExchange(newFakeOutput(), newFakeTools(voice.AnswerResult{}))
	tap := make(chan audio.Frame, 4)
	tap <- audio.Frame{PCM: ramp(0, 1600), Rate: 16000, Channels: 1}
	x.Tap = tap
	srv := newLiveServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		s, err := newTestVoice(t, srv).Open(ctx, x)
		if s != nil {
			_ = s.Close()
		}
		done <- err
	}()
	c := srv.conn()
	c.expect(evSessionStart)
	cancel()
	if err := returns(t, done, time.Second, "Open"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Open whose context ended before the start = %v, want context.Canceled", err)
	}
	c.closed()
	for {
		select {
		case ev := <-c.in:
			if ev.Type == evInputAudioAppend {
				t.Fatal("a session whose exchange ended before it started sent room audio")
			}
			continue
		default:
		}
		break
	}
}

func TestOpenRefusesAnExchangeWithoutToolsOrOutput(t *testing.T) {
	srv := newLiveServer(t)
	v := newTestVoice(t, srv)
	x := testExchange(newFakeOutput(), nil)
	if _, err := v.Open(context.Background(), x); err == nil || !strings.Contains(err.Error(), "tools") {
		t.Errorf("Open without tools = %v, want a refusal", err)
	}
	x = testExchange(nil, newFakeTools(voice.AnswerResult{}))
	x.Out = nil
	if _, err := v.Open(context.Background(), x); err == nil || !strings.Contains(err.Error(), "output") {
		t.Errorf("Open without an output = %v, want a refusal", err)
	}
}

func TestOpenSendsTheConfiguredVoice(t *testing.T) {
	srv := newLiveServer(t)
	v, err := New(Config{URL: srv.url(), Voice: "marin", Token: func(context.Context) (string, error) { return "access-token", nil }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	opened := make(chan voice.Session, 1)
	go func() {
		s, _ := v.Open(context.Background(), testExchange(newFakeOutput(), newFakeTools(voice.AnswerResult{})))
		opened <- s
	}()
	start := srv.conn().start()
	if s := <-opened; s != nil {
		defer func() { _ = s.Close() }()
	}
	if start.Session.Audio.Output == nil || start.Session.Audio.Output.Voice != "marin" {
		t.Fatalf("session.start %s does not name the configured voice", start.raw)
	}

	_, _, start = openSession(t, srv, testExchange(newFakeOutput(), newFakeTools(voice.AnswerResult{})))
	if start.Session.Audio.Output != nil {
		t.Fatalf("session.start %s names a voice nobody configured", start.raw)
	}
}

func TestTheTapDownmixesTwoChannels(t *testing.T) {
	r := resampler{to: SampleRate}
	got := r.convert(audio.Frame{PCM: []int16{1000, 3000, 1000, 3000}, Rate: SampleRate, Channels: 2})
	if fmt.Sprint(got) != "[2000 2000]" {
		t.Fatalf("a two-channel frame became %v, want its channels averaged", got)
	}
}

func TestTheTapRestartsAtANewRate(t *testing.T) {
	r := resampler{to: SampleRate}
	first := r.convert(audio.Frame{PCM: ramp(0, 1600), Rate: 16000, Channels: 1})
	second := r.convert(audio.Frame{PCM: ramp(0, 4800), Rate: 48000, Channels: 1})
	if len(first) < 2398 || len(second) < 2398 || len(second) > 2400 {
		t.Fatalf("100 ms at 16 kHz gave %d samples and 100 ms at 48 kHz gave %d, want about 2,400 each at 24 kHz", len(first), len(second))
	}
}

func TestChunksCutTextWithNoSpacesOnARuneBoundary(t *testing.T) {
	text := strings.Repeat("€", 500) // 1,500 bytes, no space
	got := chunks(text)
	for _, c := range got {
		if len(c) > appendLimit || !utf8.ValidString(c) {
			t.Fatalf("chunk of %d bytes, valid UTF-8 %v", len(c), utf8.ValidString(c))
		}
	}
	if strings.Join(got, "") != text {
		t.Fatal("the chunks do not add up to the text")
	}
}

func TestChunksCutTextWithNoSentenceEndAtASpace(t *testing.T) {
	words := make([]string, 300)
	for i := range words {
		// 8 bytes and a space: 1,000 bytes is not a word boundary, so a
		// hard cut there would split a word.
		words[i] = fmt.Sprintf("word%04d", i)
	}
	text := strings.Join(words, " ")
	got := chunks(text)
	if len(got) < 2 || strings.Join(got, " ") != text {
		t.Fatalf("%d chunks that do not add up to the text at spaces", len(got))
	}
	for _, c := range got {
		if len(c) > appendLimit {
			t.Fatalf("chunk of %d bytes, over the limit", len(c))
		}
		for _, w := range strings.Fields(c) {
			if len(w) != 8 || !strings.HasPrefix(w, "word") {
				t.Fatalf("chunk cuts a word: %q", w)
			}
		}
	}
}

func TestTheAskersWordsAreCutToHalfTheHistory(t *testing.T) {
	x := testExchange(newFakeOutput(), newFakeTools(voice.AnswerResult{}))
	x.AskerTurn = strings.Repeat("word ", 4000) // 20 KB
	text := seedText(t, history(x))
	if len(text) > historyBudget || !strings.Contains(text, "The person calling you has said so far: word") {
		t.Fatalf("history is %d bytes, want at most %d with the asker's words cut", len(text), historyBudget)
	}
}

// A newest line longer than the whole budget is cut to fit, keeping its last
// words, and the older lines are left out so the transcript stays whole.
func TestAnOverlongNewestLineIsCutToFit(t *testing.T) {
	x := testExchange(newFakeOutput(), newFakeTools(voice.AnswerResult{}))
	x.AskerTurn = ""
	x.Recent = []voice.TurnText{
		{Speaker: 0, Text: "first line"},
		{Speaker: 1, Text: strings.Repeat("blah ", 5000) + "the very end"},
	}
	text := seedText(t, history(x))
	if len(text) > historyBudget || !strings.HasSuffix(text, "the very end") || strings.Contains(text, "first line") {
		t.Fatalf("history of %d bytes ends %q, want the newest line's end and nothing older", len(text), text[max(0, len(text)-20):])
	}
}

// Nothing the session logs holds a token or a word of the room.
func TestNoTokenAndNoRoomWordsReachTheLog(t *testing.T) {
	defer func(d time.Duration) { sayStartTimeout = d }(sayStartTimeout)
	sayStartTimeout = 200 * time.Millisecond
	var logs bytes.Buffer
	srv := newLiveServer(t)
	v, err := New(Config{URL: srv.url(), Logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})), Token: func(context.Context) (string, error) { return "access-token", nil }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tools := newFakeTools(voice.AnswerResult{})
	tools.err = errors.New(`dispatch search "the secret plan": HTTP 500`)
	x := testExchange(newFakeOutput(), tools)
	x.Recent = append(x.Recent, voice.TurnText{Speaker: 2, Text: "the secret plan"})
	opened := make(chan voice.Session, 1)
	go func() {
		s, _ := v.Open(context.Background(), x)
		opened <- s
	}()
	c := srv.conn()
	c.start()
	s := <-opened
	defer func() { _ = s.Close() }()

	c.heard("Wispr, tell me the secret plan.")
	c.delegate("item_s")
	c.expect(evThinkingAppend)                                            // the failed tool's silent context
	_ = s.Say(context.Background(), "Say the secret plan.", voice.Answer) // fails: nobody speaks
	c.send(map[string]any{"type": evError, "event_id": "event_err", "error": map[string]any{"type": "invalid_request_error", "code": "bad", "message": "you sent the secret plan"}})
	waitEvent(t, s, voice.EventErr)
	_ = c.ws.Close()
	waitEvent(t, s, voice.EventErr)
	if strings.Contains(logs.String(), "secret plan") || strings.Contains(logs.String(), "access-token") {
		t.Fatalf("the log holds a room word or the token:\n%s", logs.String())
	}
	if logs.Len() == 0 {
		t.Fatal("the session logged nothing, so this test read nothing")
	}
}
