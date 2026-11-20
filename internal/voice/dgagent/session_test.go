package dgagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dvonthenen/websocket"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// TestOpenAppliesTheVoiceSettings fails if Open does not send, first and
// after Welcome, the Settings voice C is built on: the key, opted out of
// model training, room audio in at the tap's 16 kHz, Aura-2 out at the rate
// it declares to the Output, Flux listening, models Deepgram hosts thinking
// (its own first, a managed one behind it), no Think Live, and one
// client-side function, answer, through which the Concierge answers every
// question.
func TestOpenAppliesTheVoiceSettings(t *testing.T) {
	s := startStandIn(t)
	cfg := Config{APIKey: standInKey, Keyterms: []string{"Wispr", "Ghost Wispr"}}
	_, c, settings := open(t, s, cfg, voice.Exchange{SessionID: "sess-1", Tools: tools(nil), Out: newFakeOutput()})

	if got := c.header.Get("Authorization"); got != "token "+standInKey {
		t.Errorf("Authorization = %q, want the configured key as a Deepgram token", got)
	}
	checks := []struct {
		path []string
		want any
	}{
		{[]string{"mip_opt_out"}, true},
		{[]string{"audio", "input", "encoding"}, "linear16"},
		{[]string{"audio", "input", "sample_rate"}, float64(16000)},
		{[]string{"audio", "output", "encoding"}, "linear16"},
		{[]string{"audio", "output", "sample_rate"}, float64(OutputRate)},
		{[]string{"audio", "output", "container"}, "none"},
		{[]string{"agent", "listen", "provider", "type"}, "deepgram"},
		{[]string{"agent", "listen", "provider", "model"}, "flux-general-en"},
		{[]string{"agent", "listen", "provider", "version"}, "v2"},
		{[]string{"agent", "speak", "provider", "type"}, "deepgram"},
		{[]string{"agent", "speak", "provider", "version"}, "v1"},
		{[]string{"agent", "speak", "provider", "model"}, DefaultSpeakModel},
	}
	for _, ck := range checks {
		if got := field(settings, ck.path...); got != ck.want {
			t.Errorf("Settings %s = %v, want %v", strings.Join(ck.path, "."), got, ck.want)
		}
	}
	thinks := thinkModels(t, settings)
	want := []string{DefaultThinkModel, fallbackThinkModel}
	if !slices.Equal(thinkNames(thinks), want) {
		t.Fatalf("Settings think = %v, want %v, in order", thinkNames(thinks), want)
	}
	if !strings.HasPrefix(DefaultThinkModel, "deepgram/") || strings.HasPrefix(fallbackThinkModel, "deepgram/") {
		t.Errorf("think models %s then %s, want Deepgram's own first and a managed one behind it", DefaultThinkModel, fallbackThinkModel)
	}
	for i, think := range thinks {
		if think["endpoint"] != nil {
			t.Errorf("think %d names an endpoint; the model must be one Deepgram hosts, since the unit has no public address", i)
		}
		if think["prompt"] != thinks[0]["prompt"] {
			t.Errorf("think %d has another prompt than think 0", i)
		}
		checkAnswerFunction(t, think)
	}
	if !strings.HasPrefix(DefaultSpeakModel, "aura-2-") {
		t.Errorf("DefaultSpeakModel = %q, want an Aura-2 voice", DefaultSpeakModel)
	}
	keyterms, _ := field(settings, "agent", "listen", "provider", "keyterms").([]any)
	if len(keyterms) != 2 || keyterms[0] != "Wispr" || keyterms[1] != "Ghost Wispr" {
		t.Errorf("listen keyterms = %v, want the configured ones", keyterms)
	}
	// No Think Live: voice C uses none of it (its fillers, backchannels,
	// presence checks and frustration events are all off), and its user
	// backchannel detection can hold the room's speech during the agent's
	// speech for a backchannel. The end-to-end proof at 66b984b heard the
	// room's follow-up question go unheard in 5 of 5 runs while Deepgram
	// streamed the model's own muted reply.
	if got := field(settings, "agent", "think_live"); got != nil {
		t.Errorf("Settings agent.think_live = %v, want none", got)
	}
	for i, think := range thinks {
		for _, fn := range think["functions"].([]any) {
			f, _ := fn.(map[string]any)
			for _, k := range []string{"initial_agent_filler", "agent_filler_max_count"} {
				if _, ok := f[k]; ok {
					t.Errorf("think %d function %v sets Think Live's %s, want none", i, f["name"], k)
				}
			}
		}
	}
}

// thinkModels returns the Settings' think models, in order.
func thinkModels(t *testing.T, settings map[string]any) []map[string]any {
	t.Helper()
	list, _ := field(settings, "agent", "think").([]any)
	if len(list) == 0 {
		t.Fatalf("Settings think = %v, want a list of models", field(settings, "agent", "think"))
	}
	thinks := make([]map[string]any, len(list))
	for i, th := range list {
		thinks[i], _ = th.(map[string]any)
	}
	return thinks
}

// thinkNames names each think model as provider/model.
func thinkNames(thinks []map[string]any) []string {
	names := make([]string, len(thinks))
	for i, think := range thinks {
		names[i] = fmt.Sprint(field(think, "provider", "type"), "/", field(think, "provider", "model"))
	}
	return names
}

// checkAnswerFunction fails unless think declares one function, the
// contract's answer tool taking the asker's question, client-side, and held
// to the end of the turn: the Concierge may send a question to an agent,
// which cannot be taken back.
func checkAnswerFunction(t *testing.T, think map[string]any) {
	t.Helper()
	functions, _ := think["functions"].([]any)
	if len(functions) != 1 {
		t.Fatalf("Settings declare %d functions %v, want the answer tool alone", len(functions), functions)
	}
	fn, _ := functions[0].(map[string]any)
	if fn["name"] != voice.AnswerTool || fn["description"] == "" {
		t.Errorf("function = %v, want %s with a description", fn, voice.AnswerTool)
	}
	if fn["endpoint"] != nil {
		t.Errorf("function %v has an endpoint; it must run client-side", fn["name"])
	}
	if got := field(fn, "parameters", "properties", "question", "type"); got != "string" {
		t.Errorf("answer's question parameter has type %v, want string", got)
	}
	if required, _ := field(fn, "parameters", "required").([]any); len(required) != 1 || required[0] != "question" {
		t.Errorf("answer requires %v, want the question", field(fn, "parameters", "required"))
	}
	if fn["defer_until_eot"] != true {
		t.Errorf("defer_until_eot = %v, want the call held to the end of the turn", fn["defer_until_eot"])
	}
}

// TestTheThinkModelComesFromTheSetting fails if the model named in the
// setting does not think for the voice, with the managed model behind it
// unless it is that model, or if a name that is not provider/model, or
// deepgram/think-fast, which refuses mip_opt_out, is taken.
func TestTheThinkModelComesFromTheSetting(t *testing.T) {
	for _, c := range []struct {
		model string
		want  []string
	}{
		{"google/gemini-3.1-flash-lite", []string{"google/gemini-3.1-flash-lite", fallbackThinkModel}},
		{fallbackThinkModel, []string{fallbackThinkModel}},
	} {
		s := startStandIn(t)
		_, _, settings := open(t, s, Config{ThinkModel: c.model}, voice.Exchange{Out: newFakeOutput(), Tools: tools(nil)})
		if got := thinkNames(thinkModels(t, settings)); !slices.Equal(got, c.want) {
			t.Errorf("think_model %s: Settings think = %v, want %v", c.model, got, c.want)
		}
	}
	for _, bad := range []string{"gemini-3.1-flash-lite", "google/", "/gemini", "google /gemini", "deepgram/think-fast"} {
		if _, err := New(&Config{APIKey: "k", ThinkModel: bad}); err == nil {
			t.Errorf("New with think model %q succeeded, want an error", bad)
		}
	}
}

// TestOpenPutsTheMeetingInThePrompt fails if the think prompt does not send
// every question to the answer function, or lacks the recent labelled
// transcript and the asker's words so far, which it needs to hear the
// question in context.
func TestOpenPutsTheMeetingInThePrompt(t *testing.T) {
	s := startStandIn(t)
	at := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	x := voice.Exchange{
		SessionID: "sess-1",
		AskerTurn: "what is happening with ABC-12",
		Recent: []voice.TurnText{
			{Speaker: 0, Text: "Let us look at the voice work.", At: at},
			{Speaker: -1, Text: "Sure.", At: at.Add(time.Second)},
		},
		Out: newFakeOutput(), Tools: tools(nil),
	}
	_, _, settings := open(t, s, Config{}, x)
	prompt, _ := thinkModels(t, settings)[0]["prompt"].(string)
	for _, want := range []string{
		"Speaker 0: Let us look at the voice work.",
		"Unknown speaker: Sure.",
		"what is happening with ABC-12",
		"call the " + voice.AnswerTool + " function",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("think prompt lacks %q:\n%s", want, prompt)
		}
	}
}

// TestThePromptLeavesSideTalkUnanswered fails if the think prompt tells the
// model to call the answer function with words that were not a question to
// it. The real end-to-end proof at ace991b heard "Okay, let's move on to the
// budget review next." said after an answer handed to the Concierge, and
// answered, because the prompt said to call the function "with what you
// heard" whenever the model did not catch a question.
func TestThePromptLeavesSideTalkUnanswered(t *testing.T) {
	for _, bad := range []string{"if you did not catch the question, call"} {
		if strings.Contains(DefaultPrompt, bad) {
			t.Errorf("the think prompt says %q, which sends side talk to the answer function", bad)
		}
	}
	for _, want := range []string{
		"Only a question or request said to you gets a call",
		"moving on to another topic",
		"Do not turn them into a question",
	} {
		if !strings.Contains(DefaultPrompt, want) {
			t.Errorf("the think prompt lacks %q", want)
		}
	}
}

// TestOpenWaitsForSettingsApplied fails if Open returns, or the session
// sends audio, before Deepgram confirms the Settings: the docs forbid audio
// before SettingsApplied.
func TestOpenWaitsForSettingsApplied(t *testing.T) {
	s := startStandIn(t)
	agent, err := New(&Config{APIKey: "k", URL: s.url})
	if err != nil {
		t.Fatal(err)
	}
	tap := make(chan audio.Frame, 1)
	tap <- audio.Frame{PCM: []int16{1, 2, 3}, Rate: 16000, Channels: 1}
	done := make(chan error, 1)
	var sess voice.Session
	go func() {
		var err error
		sess, err = agent.Open(context.Background(), &voice.Exchange{Tap: tap, Out: newFakeOutput(), Tools: tools(nil)})
		done <- err
	}()
	c := s.accept(t)
	c.expect(t, "Settings")
	select {
	case err := <-done:
		t.Fatalf("Open returned %v before SettingsApplied", err)
	case <-time.After(200 * time.Millisecond):
	}
	if f, ok := c.next(t, 50*time.Millisecond); ok {
		t.Fatalf("the session sent %v (%s) before SettingsApplied", f.typ, f.raw)
	}
	c.send(map[string]any{"type": "SettingsApplied"})
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Open did not return after SettingsApplied")
	}
	defer func() { _ = sess.Close() }()
	f, ok := c.next(t, 2*time.Second)
	if !ok || !f.binary || !bytes.Equal(f.raw, pcm16(1, 2, 3)) {
		t.Fatalf("after SettingsApplied the session sent %+v, want the tap's audio as PCM16", f)
	}
}

// TestOpenFailsOnASettingsError fails if Open succeeds when Deepgram
// refuses the Settings, or hides the code it gave.
func TestOpenFailsOnASettingsError(t *testing.T) {
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
	c.send(map[string]any{"type": "Error", "code": "INVALID_SETTINGS", "description": "think model rejects mip_opt_out"})
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "INVALID_SETTINGS") {
			t.Fatalf("Open = %v, want an error naming INVALID_SETTINGS", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Open did not return after Deepgram's Error")
	}
}

// TestNewRefusesNoKey fails if a voice without a Deepgram key can be built.
func TestNewRefusesNoKey(t *testing.T) {
	if _, err := New(&Config{}); err == nil {
		t.Fatal("New with no API key succeeded")
	}
}

// TestOpenRefusesAnExchangeWithNoOutput fails if a session opens with no
// Output to speak through, which would fail only once the agent spoke.
func TestOpenRefusesAnExchangeWithNoOutput(t *testing.T) {
	s := startStandIn(t)
	agent, err := New(&Config{APIKey: "k", URL: s.url})
	if err != nil {
		t.Fatal(err)
	}
	// The stand-in plays the whole handshake, so only the session's own
	// check can refuse.
	go func() {
		select {
		case c := <-s.conns:
			<-c.frames // Settings
			c.send(map[string]any{"type": "SettingsApplied"})
		case <-time.After(3 * time.Second):
		}
	}()
	if sess, err := agent.Open(context.Background(), &voice.Exchange{Tools: tools(nil)}); err == nil {
		_ = sess.Close()
		t.Fatal("Open with no Output succeeded")
	}
}

// answerCall is Deepgram calling the answer tool with question.
func answerCall(id, question string) map[string]any {
	args, _ := json.Marshal(voice.AnswerArgs{Question: question})
	return map[string]any{"type": "FunctionCallRequest", "functions": []any{
		map[string]any{"id": id, "name": voice.AnswerTool, "arguments": string(args), "client_side": true},
	}}
}

// answerOf is a Concierge answer as the Supervisor's tools return it.
func answerOf(say, context string) json.RawMessage {
	data, _ := json.Marshal(voice.AnswerResult{Say: say, Context: context})
	return data
}

// responseContent decodes a FunctionCallResponse's content.
func responseContent(t *testing.T, resp map[string]any) map[string]any {
	t.Helper()
	raw, _ := resp["content"].(string)
	var content map[string]any
	if err := json.Unmarshal([]byte(raw), &content); err != nil {
		t.Fatalf("FunctionCallResponse content %q is not a JSON object: %v", raw, err)
	}
	return content
}

// TestAnAnswerIsSpokenWordForWord fails if a call to the answer tool does
// not run it through the exchange's tools with the call's id and arguments,
// report it as a ToolCall event, hand the Concierge's text to Deepgram word
// for word with InjectAgentMessage, and then tell the model it was answered,
// without the text or the context, so the model has nothing to say. The
// model is told at once, without waiting for Deepgram to speak the words:
// Deepgram queues an injection behind the model's reply in flight, which
// waits for this answer (the real probe sat 30 s in that wait).
func TestAnAnswerIsSpokenWordForWord(t *testing.T) {
	s := startStandIn(t)
	got := make(chan voice.ToolCall, 1)
	const say = "ABC-12 is in review. PR 84 holds it."
	runner := tools(func(_ context.Context, call voice.ToolCall) (json.RawMessage, error) {
		got <- call
		return answerOf(say, "last changed 40 minutes ago"), nil
	})
	sess, c, _ := open(t, s, Config{}, voice.Exchange{Out: newFakeOutput(), Tools: runner})

	request := answerCall("fc_1", "what about ABC-12")
	request["functions"] = append([]any{map[string]any{"id": "fc_server", "name": "end_call", "arguments": "{}", "client_side": false}},
		request["functions"].([]any)...)
	c.send(request)
	ev := waitEvent(t, sess, voice.EventToolCall)
	if ev.Call.ID != "fc_1" || ev.Call.Name != voice.AnswerTool || string(ev.Call.Args) != `{"question":"what about ABC-12"}` {
		t.Errorf("ToolCall event = %+v, want fc_1 answer with its arguments", ev.Call)
	}
	select {
	case call := <-got:
		if call.ID != "fc_1" || call.Name != voice.AnswerTool || string(call.Args) != `{"question":"what about ABC-12"}` {
			t.Errorf("the tools ran %+v, want fc_1 answer with its arguments", call)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the tools ran nothing")
	}
	msg := c.expect(t, "InjectAgentMessage")
	if msg["message"] != say || msg["behavior"] != "queue" {
		t.Errorf("InjectAgentMessage = %v, want the Concierge's text, queued", msg)
	}
	resp := c.expect(t, "FunctionCallResponse") // no echo yet: Deepgram may hold it for this
	if resp["id"] != "fc_1" || resp["name"] != voice.AnswerTool {
		t.Errorf("FunctionCallResponse = %v, want fc_1 answer", resp)
	}
	content := responseContent(t, resp)
	if content["answered"] != true || len(content) != 1 {
		t.Errorf("FunctionCallResponse content = %v, want answered alone: a fact the model can read is one it speaks (the real probe: gpt-4o-mini spoke the context)", content)
	}
	if raw, _ := resp["content"].(string); strings.Contains(raw, "PR 84") || strings.Contains(raw, "40 minutes") {
		t.Errorf("FunctionCallResponse content %s gives the model words to say", raw)
	}
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": say, "conversational_behavior": "default"})
	c.quiet(t, 200*time.Millisecond, "FunctionCallResponse")
	select {
	case call := <-got:
		t.Errorf("the tools ran the server-side function %+v", call)
	default:
	}
}

// TestAnEmptyAnswerIsNotHandedOver fails if an answer with nothing to say
// is injected, or the model is told it was answered.
func TestAnEmptyAnswerIsNotHandedOver(t *testing.T) {
	s := startStandIn(t)
	runner := tools(func(context.Context, voice.ToolCall) (json.RawMessage, error) {
		return answerOf("", "nothing found"), nil
	})
	_, c, _ := open(t, s, Config{}, voice.Exchange{Out: newFakeOutput(), Tools: runner})
	c.send(answerCall("fc_e", "what about ABC-13"))
	f := c.nextText(t, 3*time.Second)
	if f.typ != "FunctionCallResponse" {
		t.Fatalf("for an answer with nothing to say the session sent %s (%s), want only the response", f.typ, f.raw)
	}
	var resp map[string]any
	_ = json.Unmarshal(f.raw, &resp)
	if content := responseContent(t, resp); content["answered"] != false || len(content) != 1 {
		t.Errorf("for an answer with nothing to say the model was told %v, want answered false alone", content)
	}
}

// TestTheModelsOwnWordsWhileItsAnswerIsOutAreNotPlayed fails if words the
// model says with its answer call reach the speaker, or a filler Deepgram
// words itself, or if a line the Supervisor says while the answer is found
// (the Concierge's routing line, forwarded to Say) does not play. The real
// probe heard phonellm say "The issue is held by Lucia." while the
// Concierge was finding that Ada holds it, and the proof heard Think Live
// fill with "Let me connect you to someone who can help."
func TestTheModelsOwnWordsWhileItsAnswerIsOutAreNotPlayed(t *testing.T) {
	setSpeechGap(t, time.Minute)
	s := startStandIn(t)
	out := newFakeOutput()
	release := make(chan struct{})
	runner := tools(func(context.Context, voice.ToolCall) (json.RawMessage, error) {
		<-release
		return answerOf("Ada holds it.", ""), nil
	})
	sess, c, _ := open(t, s, Config{}, voice.Exchange{Out: out, Tools: runner})
	c.send(map[string]any{"type": "ConversationText", "role": "user", "content": "who holds it"})
	c.send(answerCall("fc_n", "who holds it"))
	waitEvent(t, sess, voice.EventToolCall)
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "The issue is held by Lucia.", "conversational_behavior": "default"})
	c.sendAudio(tone(OutputRate, 40*time.Millisecond, 100))
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "Let me connect you to someone who can help.", "conversational_behavior": "filler"})
	c.sendAudio(tone(OutputRate, 40*time.Millisecond, 150))
	routing := sayAsync(sess, "Reading ABC-12.") // the Supervisor's Ack line
	if msg := c.expect(t, "InjectAgentMessage"); msg["message"] != "Reading ABC-12." {
		t.Fatalf("while the answer was found Say sent %v, want its words", msg)
	}
	line := tone(OutputRate, 20*time.Millisecond, 200)
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "Reading ABC-12."})
	c.sendAudio(line)
	c.send(map[string]any{"type": "AgentAudioDone"})
	if err := returned(t, routing); err != nil {
		t.Errorf("Say of the routing line: %v", err)
	}
	close(release)
	if msg := c.expect(t, "InjectAgentMessage"); msg["message"] != "Ada holds it." {
		t.Fatalf("after the routing line the session sent %v, want the answer", msg)
	}
	answer := tone(OutputRate, 20*time.Millisecond, 300)
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "Ada holds it.", "conversational_behavior": "default"})
	c.sendAudio(answer)
	c.send(map[string]any{"type": "AgentAudioDone"})
	if got := out.nextPlayback(t).written(); !bytes.Equal(got, line) {
		t.Errorf("the speaker got %d bytes first, want the routing line's %d, without the model's own words or Deepgram's filler", len(got), len(line))
	}
	p := out.nextPlayback(t)
	p.waitBytes(t, len(answer))
	if got := p.written(); !bytes.Equal(got, answer) {
		t.Errorf("the speaker got %d bytes next, want the answer's %d", len(got), len(answer))
	}
}

// TestAnInjectionCutsTheModelsOwnReply fails if the answer is queued behind
// a reply of the model's own that is coming and does not play, if an
// injection interrupts while an answer call is pending, or if an injection
// cuts anything when no such reply is coming. The end-to-end proof at
// 6ed0386 saw the model say "Let me look that up." with its call: Deepgram
// streamed the muted reply in real time, the queued answer waited behind
// it, and the exchange ended idle before the answer played. At 66b984b an
// interrupting routing line, said while the call was pending, made Deepgram
// cancel the call (FunctionCallCancelled 66 ms later), and the room heard
// nothing. So a line said during a call queues, and the answer cuts the
// muted reply only once the call is answered.
func TestAnInjectionCutsTheModelsOwnReply(t *testing.T) {
	setSpeechGap(t, time.Minute)
	s := startStandIn(t)
	release := make(chan struct{})
	runner := tools(func(context.Context, voice.ToolCall) (json.RawMessage, error) {
		<-release
		return answerOf("Ada holds it.", ""), nil
	})
	var logs lockedBuf
	sess, c, _ := open(t, s, Config{Logger: slog.New(slog.NewTextHandler(&logs, nil)), ackLimit: 200 * time.Millisecond}, voice.Exchange{Out: newFakeOutput(), Tools: runner})
	c.send(map[string]any{"type": "ConversationText", "role": "user", "content": "who holds it"})
	c.send(answerCall("fc_e", "who holds it"))
	waitEvent(t, sess, voice.EventToolCall)
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "Let me look that up.", "conversational_behavior": "default"})
	c.sendAudio(tone(OutputRate, 40*time.Millisecond, 100))
	// The answer returns once the session has taken the reply for the
	// model's own, as it does live: the reply streams while the Concierge
	// finds the answer.
	for deadline := time.Now().Add(3 * time.Second); !strings.Contains(logs.String(), "the model's own words while it has an answer are not played"); {
		if time.Now().After(deadline) {
			t.Fatal("the session did not take the reply for the model's own")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// The routing line, said while the call is pending, queues.
	routing := make(chan error, 1)
	go func() { routing <- sess.Say(context.Background(), "Reading the record.", voice.Ack) }()
	if msg := c.expect(t, "InjectAgentMessage"); msg["behavior"] != "queue" {
		t.Fatalf("while the answer call was pending the session sent %v, want behavior queue: an interrupt cancels the call", msg)
	}
	_ = returned(t, routing)
	close(release)
	// The call is answered first, then the answer cuts the muted reply.
	if content := responseContent(t, c.expect(t, "FunctionCallResponse")); content["answered"] != true {
		t.Fatalf("FunctionCallResponse content = %v, want answered true", content)
	}
	if msg := c.expect(t, "InjectAgentMessage"); msg["message"] != "Ada holds it." || msg["behavior"] != "interrupt" {
		t.Fatalf("with the model's own reply coming the session sent %v, want the answer with behavior interrupt", msg)
	}
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "Ada holds it."})
	c.sendAudio(tone(OutputRate, 20*time.Millisecond, 300))
	c.send(map[string]any{"type": "AgentAudioDone"})
	// No reply of the model's own is coming now: the next injection queues.
	_ = sayAsync(sess, "The room test is today.")
	if msg := c.expect(t, "InjectAgentMessage"); msg["behavior"] != "queue" {
		t.Fatalf("with nothing of the model's own coming the session sent %v, want behavior queue", msg)
	}
}

// TestNoLineOfItsOwnWhileTheAnswerIsFound fails if voice C says anything of
// its own while the Concierge finds an answer: the Concierge words the one
// routing line (spec R6), and the Supervisor says it.
func TestNoLineOfItsOwnWhileTheAnswerIsFound(t *testing.T) {
	s := startStandIn(t)
	release := make(chan struct{})
	runner := tools(func(context.Context, voice.ToolCall) (json.RawMessage, error) {
		<-release
		return answerOf("Ada holds it.", ""), nil
	})
	_, c, _ := open(t, s, Config{}, voice.Exchange{Out: newFakeOutput(), Tools: runner})
	c.send(map[string]any{"type": "ConversationText", "role": "user", "content": "who holds it"})
	c.send(answerCall("fc_w", "who holds it"))
	c.quiet(t, 2*time.Second, "InjectAgentMessage")
	close(release)
	if msg := c.expect(t, "InjectAgentMessage"); msg["message"] != "Ada holds it." {
		t.Fatalf("the session sent %v, want the answer", msg)
	}
}

// TestAnotherFunctionReachesNoTool fails if a call to a function voice C
// never declared reaches the exchange's tools, or leaves Deepgram waiting.
func TestAnotherFunctionReachesNoTool(t *testing.T) {
	s := startStandIn(t)
	ran := make(chan voice.ToolCall, 1)
	runner := tools(func(_ context.Context, call voice.ToolCall) (json.RawMessage, error) {
		ran <- call
		return answerOf("", ""), nil
	})
	_, c, _ := open(t, s, Config{}, voice.Exchange{Out: newFakeOutput(), Tools: runner})
	c.send(map[string]any{"type": "FunctionCallRequest", "functions": []any{
		map[string]any{"id": "fc_x", "name": "read_issue", "arguments": `{"key":"ABC-12"}`, "client_side": true},
	}})
	if content := responseContent(t, c.expect(t, "FunctionCallResponse")); content["error"] == nil {
		t.Errorf("a call to read_issue was answered %v, want an error", content)
	}
	select {
	case call := <-ran:
		t.Errorf("a call to an undeclared function ran %+v", call)
	default:
	}
}

// TestAFailedAnswerSaysNothingOfItsOwn fails if an answer that fails leaves
// Deepgram waiting, gives the model the error's text, or has voice C say a
// line of its own: the Supervisor says the Concierge's own sorry line.
func TestAFailedAnswerSaysNothingOfItsOwn(t *testing.T) {
	s := startStandIn(t)
	runner := tools(func(context.Context, voice.ToolCall) (json.RawMessage, error) {
		return nil, errors.New("the record cannot be reached")
	})
	_, c, _ := open(t, s, Config{}, voice.Exchange{Out: newFakeOutput(), Tools: runner})
	c.send(answerCall("fc_2", "what about ABC-12"))
	f := c.nextText(t, 3*time.Second)
	if f.typ != "FunctionCallResponse" {
		t.Fatalf("for a failed answer the session sent %s (%s) first, want only the response", f.typ, f.raw)
	}
	var resp map[string]any
	_ = json.Unmarshal(f.raw, &resp)
	if content := responseContent(t, resp); content["answered"] != false || len(content) != 1 {
		t.Errorf("FunctionCallResponse content = %v, want answered false alone", content)
	}
	if raw, _ := resp["content"].(string); strings.Contains(raw, "record") {
		t.Errorf("FunctionCallResponse %s gives the model the error's text", raw)
	}
	c.quiet(t, 300*time.Millisecond, "InjectAgentMessage")
}

// TestARefusedAnswerIsSpokenWhenTheRoomIsDone fails if an answer Deepgram
// refused because someone was speaking is lost: once their turn ends and no
// new question takes it, the answer is handed over again.
func TestARefusedAnswerIsSpokenWhenTheRoomIsDone(t *testing.T) {
	s := startStandIn(t)
	runner := tools(func(context.Context, voice.ToolCall) (json.RawMessage, error) {
		return answerOf("ABC-12 is in review.", ""), nil
	})
	_, c, _ := open(t, s, Config{retryGrace: 200 * time.Millisecond}, voice.Exchange{Out: newFakeOutput(), Tools: runner})
	c.send(map[string]any{"type": "ConversationText", "role": "user", "content": "what about ABC-12"})
	c.send(answerCall("fc_r", "what about ABC-12"))
	c.expect(t, "InjectAgentMessage")
	c.send(map[string]any{"type": "InjectionRefused"})
	c.expect(t, "FunctionCallResponse")
	c.send(map[string]any{"type": "ConversationText", "role": "user", "content": "mm-hm"})
	if msg := c.expect(t, "InjectAgentMessage"); msg["message"] != "ABC-12 is in review." {
		t.Fatalf("after the room's turn the session sent %v, want the refused answer again", msg)
	}
}

// TestARefusedAnswerIsDroppedWhenTheRoomAsksAgain fails if an answer Deepgram
// refused is handed over again after the room asked something new.
func TestARefusedAnswerIsDroppedWhenTheRoomAsksAgain(t *testing.T) {
	s := startStandIn(t)
	answers := make(chan string, 2)
	answers <- "ABC-12 is in review."
	answers <- "Ada holds it."
	runner := tools(func(context.Context, voice.ToolCall) (json.RawMessage, error) {
		return answerOf(<-answers, ""), nil
	})
	_, c, _ := open(t, s, Config{retryGrace: 300 * time.Millisecond}, voice.Exchange{Out: newFakeOutput(), Tools: runner})
	c.send(map[string]any{"type": "ConversationText", "role": "user", "content": "what about ABC-12"})
	c.send(answerCall("fc_r", "what about ABC-12"))
	c.expect(t, "InjectAgentMessage")
	c.send(map[string]any{"type": "InjectionRefused"})
	c.expect(t, "FunctionCallResponse")
	c.send(map[string]any{"type": "ConversationText", "role": "user", "content": "who holds it"})
	c.send(answerCall("fc_n", "who holds it"))
	if msg := c.expect(t, "InjectAgentMessage"); msg["message"] != "Ada holds it." {
		t.Fatalf("after a new question the session sent %v, want its answer", msg)
	}
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "Ada holds it."})
	c.quiet(t, time.Second, "InjectAgentMessage")
}

// TestAnInjectionGivenUpIsNotPlayedLater fails if words Deepgram speaks
// after the session gave up on them reach the speaker: the Supervisor keeps
// such an answer and may say it again.
func TestAnInjectionGivenUpIsNotPlayedLater(t *testing.T) {
	setSpeechGap(t, time.Minute)
	s := startStandIn(t)
	out := newFakeOutput()
	sess, c, _ := open(t, s, Config{sayLimit: 100 * time.Millisecond}, voice.Exchange{Out: out, Tools: tools(nil)})
	done := sayAsync(sess, "A late fact.")
	c.expect(t, "InjectAgentMessage")
	if err := returned(t, done); !errors.Is(err, ErrNotSpoken) {
		t.Fatalf("Say = %v, want ErrNotSpoken", err)
	}
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "A late fact."})
	c.sendAudio(tone(OutputRate, 20*time.Millisecond, 100))
	c.send(map[string]any{"type": "AgentAudioDone"})
	next := tone(OutputRate, 20*time.Millisecond, 200)
	// The voice's next words, its own and so played.
	_ = sayAsync(sess, "Next words.")
	c.expect(t, "InjectAgentMessage")
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "Next words."})
	c.sendAudio(next)
	// The first playback is the reply after the words given up, or those
	// words if they played.
	first := out.nextPlayback(t)
	first.waitBytes(t, len(next))
	if got := first.written(); !bytes.Equal(got, next) {
		t.Fatalf("the speaker got %d bytes first that are not the next reply's: the words given up played", len(got))
	}
}

// TestAGivenUpLineThatStartsTheNextIsNotTakenForIt fails if words the
// session gave up on, which Deepgram speaks first as it speaks its queue in
// order, are taken for the start of the next injection that begins with the
// same words. The end-to-end proof at 8144e62 heard the given-up routing
// line "Reading the record." taken for the start of the answer "Reading the
// record. That issue is in progress.", whose own echo then did not match
// and was muted: the room heard only the routing line.
func TestAGivenUpLineThatStartsTheNextIsNotTakenForIt(t *testing.T) {
	setSpeechGap(t, time.Minute)
	s := startStandIn(t)
	out := newFakeOutput()
	sess, c, _ := open(t, s, Config{sayLimit: 100 * time.Millisecond}, voice.Exchange{Out: out, Tools: tools(nil)})
	done := sayAsync(sess, "Reading the record.")
	c.expect(t, "InjectAgentMessage")
	if err := returned(t, done); !errors.Is(err, ErrNotSpoken) {
		t.Fatalf("Say = %v, want ErrNotSpoken", err)
	}
	next := sayAsync(sess, "Reading the record. That issue is in progress.")
	c.expect(t, "InjectAgentMessage")
	// Deepgram speaks the given-up line first, then the next injection.
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "Reading the record."})
	c.sendAudio(tone(OutputRate, 20*time.Millisecond, 100))
	c.send(map[string]any{"type": "AgentAudioDone"})
	answer := tone(OutputRate, 20*time.Millisecond, 200)
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "Reading the record."})
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "That issue is in progress."})
	c.sendAudio(answer)
	c.send(map[string]any{"type": "AgentAudioDone"})
	first := out.nextPlayback(t)
	first.waitBytes(t, len(answer))
	if got := first.written(); !bytes.Equal(got, answer) {
		t.Fatalf("the speaker got %d bytes first that are not the next injection's: the given-up line played, or the injection was muted", len(got))
	}
	if err := returned(t, next); err != nil {
		t.Fatalf("Say of the next injection = %v, want nil", err)
	}
}

// TestAnAckDeepgramHoldsBackGivesUpSoon fails if a routing line (an Ack)
// that Deepgram does not start holds the session past the Ack limit. Deepgram
// queues it behind the model's own reply to its call, which waits for the
// call's response, which waits for the Concierge, which waits for the Ack:
// the end-to-end proof at 8144e62 saw it hold for the whole 30 s Say limit.
func TestAnAckDeepgramHoldsBackGivesUpSoon(t *testing.T) {
	s := startStandIn(t)
	sess, c, _ := open(t, s, Config{sayLimit: 10 * time.Second, ackLimit: 200 * time.Millisecond}, voice.Exchange{Out: newFakeOutput(), Tools: tools(nil)})
	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- sess.Say(context.Background(), "Reading the record.", voice.Ack) }()
	c.expect(t, "InjectAgentMessage")
	if err := returned(t, done); !errors.Is(err, ErrNotSpoken) {
		t.Fatalf("Say of an Ack = %v, want ErrNotSpoken", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("an Ack Deepgram held back gave up after %v, want about its 200 ms limit", took)
	}
}

// TestAnAckDeepgramSpeaksIsSpoken fails if a routing line that Deepgram
// starts at once, under the default limits, is not reported spoken: the Ack
// limit must leave Deepgram its usual time to start.
func TestAnAckDeepgramSpeaksIsSpoken(t *testing.T) {
	setSpeechGap(t, time.Minute)
	s := startStandIn(t)
	sess, c, _ := open(t, s, Config{}, voice.Exchange{Out: newFakeOutput(), Tools: tools(nil)})
	done := make(chan error, 1)
	go func() { done <- sess.Say(context.Background(), "Reading the record.", voice.Ack) }()
	c.expect(t, "InjectAgentMessage")
	time.Sleep(100 * time.Millisecond) // as long as Deepgram takes to start an injection
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "Reading the record."})
	c.sendAudio(tone(OutputRate, 20*time.Millisecond, 100))
	c.send(map[string]any{"type": "AgentAudioDone"})
	if err := returned(t, done); err != nil {
		t.Fatalf("Say of an Ack Deepgram spoke = %v, want nil", err)
	}
}

// TestSayEndsWhenItsWordsHadAlreadyPlayed fails if a Say whose audio and
// AgentAudioDone arrive before its echo loses that buffered, authorized
// audio or waits for more audio. The gate keeps it from Output until the
// matching injection text arrives.
func TestSayEndsWhenItsWordsHadAlreadyPlayed(t *testing.T) {
	setSpeechGap(t, time.Minute)
	s := startStandIn(t)
	sess, c, _ := open(t, s, Config{}, voice.Exchange{Out: newFakeOutput(), Tools: tools(nil)})
	done := sayAsync(sess, "A fact.")
	c.expect(t, "InjectAgentMessage")
	c.sendAudio(tone(OutputRate, 20*time.Millisecond, 100))
	c.send(map[string]any{"type": "AgentAudioDone"})
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "A fact."})
	if err := returned(t, done); err != nil {
		t.Fatalf("Say: %v", err)
	}
	waitEvent(t, sess, voice.EventSpeechEnded)
}

// TestACancelledCallWaitingToSpeakSaysNothing fails if an answer call
// Deepgram cancels while its answer waits behind another injection (a held
// fact the Supervisor says) is still handed over once that one is done.
func TestACancelledCallWaitingToSpeakSaysNothing(t *testing.T) {
	setSpeechGap(t, time.Minute)
	s := startStandIn(t)
	runner := tools(func(context.Context, voice.ToolCall) (json.RawMessage, error) {
		return answerOf("Second answer, cancelled.", ""), nil
	})
	sess, c, _ := open(t, s, Config{}, voice.Exchange{Out: newFakeOutput(), Tools: runner})
	held := sayAsync(sess, "A held fact.")
	c.expect(t, "InjectAgentMessage")
	c.send(map[string]any{"type": "ConversationText", "role": "user", "content": "second question"})
	c.send(answerCall("fc_2", "second question"))
	waitEvent(t, sess, voice.EventToolCall)
	time.Sleep(100 * time.Millisecond) // the answer waits behind the held fact
	c.send(map[string]any{"type": "FunctionCallCancelled", "functions": []any{map[string]any{"id": "fc_2", "name": voice.AnswerTool}}})
	time.Sleep(50 * time.Millisecond)
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "A held fact."})
	c.sendAudio(tone(OutputRate, 20*time.Millisecond, 100))
	c.send(map[string]any{"type": "AgentAudioDone"})
	if err := returned(t, held); err != nil {
		t.Fatalf("Say of the held fact: %v", err)
	}
	c.quiet(t, 300*time.Millisecond, "InjectAgentMessage")
}

// TestTheModelsOwnReplyIsNotPlayedWhenItsAudioComesFirst fails if the
// audio of the model's own words plays when it reaches the session before
// their text, after an answer: the real probe saw audio come 17 ms first.
func TestTheModelsOwnReplyIsNotPlayedWhenItsAudioComesFirst(t *testing.T) {
	setSpeechGap(t, time.Minute)
	s := startStandIn(t)
	out := newFakeOutput()
	const say = "ABC-12 is in review."
	runner := tools(func(context.Context, voice.ToolCall) (json.RawMessage, error) { return answerOf(say, ""), nil })
	sess, c, _ := open(t, s, Config{}, voice.Exchange{Out: out, Tools: runner})
	c.send(map[string]any{"type": "ConversationText", "role": "user", "content": "what about ABC-12"})
	c.send(answerCall("fc_m", "what about ABC-12"))
	c.expect(t, "InjectAgentMessage")
	c.expect(t, "FunctionCallResponse")
	answer := tone(OutputRate, 40*time.Millisecond, 100)
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": say})
	c.sendAudio(answer)
	c.send(map[string]any{"type": "AgentAudioDone"})
	waitEvent(t, sess, voice.EventSpeechEnded)
	c.sendAudio(tone(OutputRate, 20*time.Millisecond, 200)) // before its text
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "It changed this morning."})
	c.sendAudio(tone(OutputRate, 20*time.Millisecond, 200))
	c.send(map[string]any{"type": "AgentAudioDone"})
	again := tone(OutputRate, 20*time.Millisecond, 300)
	// The voice's next words, its own and so played.
	_ = sayAsync(sess, "Next words.")
	c.expect(t, "InjectAgentMessage")
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "Next words."})
	c.sendAudio(again)
	if got := out.nextPlayback(t).written(); !bytes.Equal(got, answer) {
		t.Errorf("the first playback got %d bytes, want the answer's %d", len(got), len(answer))
	}
	next := out.nextPlayback(t)
	next.waitBytes(t, len(again))
	if got := next.written(); !bytes.Equal(got, again) {
		t.Fatalf("the next playback got %d bytes that are not the filler after it: the model's own words played", len(got))
	}
}

// TestAnAnswerWhoseAudioComesFirstPlaysWhole fails if, while the model's
// own words are held back, the start of an injected answer whose audio
// reaches the session before its echo is lost.
func TestAnAnswerWhoseAudioComesFirstPlaysWhole(t *testing.T) {
	setSpeechGap(t, time.Minute)
	s := startStandIn(t)
	out := newFakeOutput()
	const say = "ABC-12 is in review."
	runner := tools(func(context.Context, voice.ToolCall) (json.RawMessage, error) { return answerOf(say, ""), nil })
	_, c, _ := open(t, s, Config{}, voice.Exchange{Out: out, Tools: runner})
	c.send(map[string]any{"type": "ConversationText", "role": "user", "content": "what about ABC-12"})
	c.send(answerCall("fc_a", "what about ABC-12"))
	c.expect(t, "InjectAgentMessage")
	first, rest := tone(OutputRate, 20*time.Millisecond, 100), tone(OutputRate, 20*time.Millisecond, 101)
	c.sendAudio(first)
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": say})
	c.sendAudio(rest)
	p := out.nextPlayback(t)
	p.waitBytes(t, len(first)+len(rest))
	if got := p.written(); !bytes.Equal(got, append(append([]byte(nil), first...), rest...)) {
		t.Fatalf("the answer played %d bytes, want all %d in order", len(got), len(first)+len(rest))
	}
}

// TestAnEmptyQuestionTakesTheTurnsWords fails if an answer call with no
// question reaches the Concierge without the words Deepgram heard in the
// turn: the real probe saw phonellm call answer with {} first, and the
// Supervisor's fallback is the first words of the wake, not the turn.
func TestAnEmptyQuestionTakesTheTurnsWords(t *testing.T) {
	s := startStandIn(t)
	got := make(chan voice.ToolCall, 1)
	runner := tools(func(_ context.Context, call voice.ToolCall) (json.RawMessage, error) {
		got <- call
		return answerOf("", ""), nil
	})
	_, c, _ := open(t, s, Config{}, voice.Exchange{Out: newFakeOutput(), Tools: runner})
	c.send(map[string]any{"type": "ConversationText", "role": "user", "content": "Wispr, what about ABC-12?"})
	c.send(map[string]any{"type": "FunctionCallRequest", "functions": []any{
		map[string]any{"id": "fc_q", "name": voice.AnswerTool, "arguments": "{}", "client_side": true},
	}})
	select {
	case call := <-got:
		var args voice.AnswerArgs
		if err := json.Unmarshal(call.Args, &args); err != nil || args.Question != "Wispr, what about ABC-12?" {
			t.Fatalf("the Concierge was asked %s, want the turn's words", call.Args)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the Concierge was asked nothing")
	}
}

// TestFunctionCallCancelledSendsNoResponse fails if a call Deepgram
// cancels keeps running, or is answered or spoken anyway.
func TestFunctionCallCancelledSendsNoResponse(t *testing.T) {
	s := startStandIn(t)
	cancelled := make(chan struct{})
	runner := tools(func(ctx context.Context, _ voice.ToolCall) (json.RawMessage, error) {
		<-ctx.Done()
		close(cancelled)
		return answerOf("Too late.", ""), nil
	})
	sess, c, _ := open(t, s, Config{}, voice.Exchange{Out: newFakeOutput(), Tools: runner})
	c.send(answerCall("fc_3", "what about the voice"))
	waitEvent(t, sess, voice.EventToolCall)
	c.send(map[string]any{"type": "FunctionCallCancelled", "functions": []any{map[string]any{"id": "fc_3", "name": voice.AnswerTool}}})
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("the cancelled tool call kept running")
	}
	deadline := time.Now().Add(300 * time.Millisecond)
	for {
		f, ok := c.next(t, time.Until(deadline))
		if !ok {
			break
		}
		if f.typ == "FunctionCallResponse" || f.typ == "InjectAgentMessage" {
			t.Fatalf("a cancelled call was followed by %s: %s", f.typ, f.raw)
		}
	}
}

// TestDeliverSpeaksALateAnswer fails if Deliver does not speak the answer it
// is given to a call still waiting, word for word, and then answer the
// call, or if it accepts an id no call has.
func TestDeliverSpeaksALateAnswer(t *testing.T) {
	s := startStandIn(t)
	runner := tools(func(ctx context.Context, _ voice.ToolCall) (json.RawMessage, error) {
		<-ctx.Done()
		return answerOf("From the runner.", ""), nil
	})
	sess, c, _ := open(t, s, Config{}, voice.Exchange{Out: newFakeOutput(), Tools: runner})
	c.send(answerCall("fc_4", "when does it ship"))
	waitEvent(t, sess, voice.EventToolCall)
	delivered := make(chan error, 1)
	go func() { delivered <- sess.Deliver(context.Background(), "fc_4", answerOf("It ships today.", "")) }()
	if msg := c.expect(t, "InjectAgentMessage"); msg["message"] != "It ships today." {
		t.Errorf("InjectAgentMessage = %v, want the delivered answer", msg)
	}
	resp := c.expect(t, "FunctionCallResponse") // before the echo
	if resp["id"] != "fc_4" || responseContent(t, resp)["answered"] != true {
		t.Errorf("FunctionCallResponse = %v, want fc_4 answered", resp)
	}
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "It ships today.", "conversational_behavior": "default"})
	if err := returned(t, delivered); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	c.quiet(t, 300*time.Millisecond, "FunctionCallResponse")
	if err := sess.Deliver(context.Background(), "fc_4", answerOf("Again.", "")); !errors.Is(err, ErrNoPendingCall) {
		t.Errorf("Deliver to an answered call = %v, want ErrNoPendingCall", err)
	}
	if err := sess.Deliver(context.Background(), "fc_none", answerOf("None.", "")); !errors.Is(err, ErrNoPendingCall) {
		t.Errorf("Deliver to an unknown call = %v, want ErrNoPendingCall", err)
	}
}

// TestSayGivesUpWhenTheWordsAreNeverSpoken fails if a Say whose words
// Deepgram never speaks holds the floor past the session's limit, or
// returns anything but ErrNotSpoken, so the Supervisor keeps the answer.
func TestSayGivesUpWhenTheWordsAreNeverSpoken(t *testing.T) {
	s := startStandIn(t)
	sess, c, _ := open(t, s, Config{sayLimit: 200 * time.Millisecond}, voice.Exchange{Out: newFakeOutput(), Tools: tools(nil)})
	start := time.Now()
	done := sayAsync(sess, "A fact nobody echoes.")
	c.expect(t, "InjectAgentMessage")
	if err := returned(t, done); !errors.Is(err, ErrNotSpoken) {
		t.Fatalf("Say = %v, want ErrNotSpoken", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("Say gave up after %v, want about its 200 ms limit", took)
	}
}

// TestSayMatchesTheWordsNotThePunctuation fails if a Say never ends when
// Deepgram echoes its words with other punctuation or capitals.
func TestSayMatchesTheWordsNotThePunctuation(t *testing.T) {
	setSpeechGap(t, time.Minute)
	s := startStandIn(t)
	sess, c, _ := open(t, s, Config{}, voice.Exchange{Out: newFakeOutput(), Tools: tools(nil)})
	done := sayAsync(sess, "ABC-12 — moved to review; PR 84 holds it.")
	c.expect(t, "InjectAgentMessage")
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "ABC-12 moved to Review.", "conversational_behavior": "default"})
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "PR 84 holds it", "conversational_behavior": "default"})
	c.sendAudio(tone(OutputRate, 20*time.Millisecond, 300))
	c.send(map[string]any{"type": "AgentAudioDone"})
	if err := returned(t, done); err != nil {
		t.Fatalf("Say: %v", err)
	}
}

// TestSayInjectsTheExactWords fails if Say does not send InjectAgentMessage
// with the text in "message" (the field the API reads; the SDK's "content"
// closes the session), queued behind the agent's own speech, or returns
// before its speech is queued to the Output: the contract's Say returns once
// it has been, and Deepgram's echo of the words comes before their audio.
func TestSayInjectsTheExactWords(t *testing.T) {
	s := startStandIn(t)
	out := newFakeOutput()
	sess, c, _ := open(t, s, Config{}, voice.Exchange{Out: out, Tools: tools(nil)})
	const text = "ABC-12 moved to review 2 hours ago; PR 81 holds it."
	done := sayAsync(sess, text)
	msg := c.expect(t, "InjectAgentMessage")
	if msg["message"] != text || msg["behavior"] != "queue" {
		t.Errorf("InjectAgentMessage = %v, want message %q with behavior queue", msg, text)
	}
	if _, has := msg["content"]; has {
		t.Errorf("InjectAgentMessage carries content: %v", msg)
	}
	notReturned(t, done, "before Deepgram took the words")
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": text, "conversational_behavior": "default"})
	notReturned(t, done, "on the echo, before the words' audio was queued to the Output")
	speech := tone(OutputRate, 60*time.Millisecond, 500)
	c.sendAudio(speech)
	c.send(map[string]any{"type": "AgentAudioDone"})
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Say: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Say did not return within 500 ms of its audio being queued")
	}
	if plays := out.playbacks(); len(plays) != 1 || len(plays[0].written()) != len(speech) {
		t.Fatalf("when Say returned the Output held %d playbacks, want the words' %d bytes queued", len(plays), len(speech))
	}
}

// TestSayWaitsForItsOwnWords fails if Say ends on the rest of a reply the
// agent was speaking when the words were queued behind it.
func TestSayWaitsForItsOwnWords(t *testing.T) {
	s := startStandIn(t)
	sess, c, _ := open(t, s, Config{}, voice.Exchange{Out: newFakeOutput(), Tools: tools(nil)})
	done := sayAsync(sess, "The holder answered: it ships today.")
	c.expect(t, "InjectAgentMessage")
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "It is in review.", "conversational_behavior": "default"})
	c.sendAudio(tone(OutputRate, 20*time.Millisecond, 100))
	c.send(map[string]any{"type": "AgentAudioDone"})
	notReturned(t, done, "on the rest of the agent's own reply")
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "The holder answered: it ships today.", "conversational_behavior": "default"})
	c.sendAudio(tone(OutputRate, 20*time.Millisecond, 200))
	c.send(map[string]any{"type": "AgentAudioDone"})
	if err := returned(t, done); err != nil {
		t.Fatalf("Say: %v", err)
	}
}

// TestSayOfSeveralSentencesMatchesTheSplitEcho fails if a Say of several
// sentences never returns when Deepgram echoes the words one sentence at a
// time, as it reports the agent's own replies.
func TestSayOfSeveralSentencesMatchesTheSplitEcho(t *testing.T) {
	s := startStandIn(t)
	sess, c, _ := open(t, s, Config{}, voice.Exchange{Out: newFakeOutput(), Tools: tools(nil)})
	done := sayAsync(sess, "ABC-12 is in review.  PR 84 holds the Deepgram voice.")
	c.expect(t, "InjectAgentMessage")
	for _, sentence := range []string{"ABC-12 is in review.", "PR 84 holds the Deepgram voice."} {
		c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": sentence, "conversational_behavior": "default"})
		c.sendAudio(tone(OutputRate, 20*time.Millisecond, 300))
	}
	c.send(map[string]any{"type": "AgentAudioDone"})
	if err := returned(t, done); err != nil {
		t.Fatalf("Say: %v", err)
	}
}

// TestInterruptEndsAWaitingSay fails if a Say whose words the Supervisor
// interrupts, before Deepgram takes them or while they play, does not
// return ErrInterrupted at once.
func TestInterruptEndsAWaitingSay(t *testing.T) {
	setSpeechGap(t, time.Minute)
	s := startStandIn(t)
	sess, c, _ := open(t, s, Config{}, voice.Exchange{Out: newFakeOutput(), Tools: tools(nil)})

	done := sayAsync(sess, "Before the echo.")
	c.expect(t, "InjectAgentMessage")
	sess.Interrupt()
	if err := returned(t, done); !errors.Is(err, ErrInterrupted) {
		t.Errorf("Say interrupted before its echo = %v, want ErrInterrupted", err)
	}

	done = sayAsync(sess, "While it plays.")
	c.expect(t, "InjectAgentMessage")
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "While it plays.", "conversational_behavior": "default"})
	c.sendAudio(tone(OutputRate, 40*time.Millisecond, 300))
	waitEvent(t, sess, voice.EventSpeechStarted)
	sess.Interrupt()
	if err := returned(t, done); !errors.Is(err, ErrInterrupted) {
		t.Errorf("Say interrupted while it played = %v, want ErrInterrupted", err)
	}
}

// TestSayReportsARefusal fails if Say does not return ErrRefused when
// Deepgram refuses the injection because someone holds the floor, by
// either of the two messages its docs name.
func TestSayReportsARefusal(t *testing.T) {
	s := startStandIn(t)
	sess, c, _ := open(t, s, Config{}, voice.Exchange{Out: newFakeOutput(), Tools: tools(nil)})
	for _, refusal := range []map[string]any{
		{"type": "InjectionRefused", "message": "user is speaking"},
		{"type": "Warning", "code": "INJECT_AGENT_MESSAGE_DURING_USER_SPEECH", "description": "ignored"},
	} {
		time.Sleep(2 * refusalEcho) // a refusal of its own, not the echo of the last
		done := sayAsync(sess, "The holder answered.")
		c.expect(t, "InjectAgentMessage")
		c.send(refusal)
		if err := returned(t, done); !errors.Is(err, ErrRefused) {
			t.Errorf("Say after %v = %v, want ErrRefused", refusal["type"], err)
		}
	}
}

// TestARefusalEndsOnlyItsOwnSay fails if Deepgram's two messages for one
// refused injection (InjectionRefused and the INJECT_AGENT_MESSAGE warning)
// end the next Say too.
func TestARefusalEndsOnlyItsOwnSay(t *testing.T) {
	s := startStandIn(t)
	sess, c, _ := open(t, s, Config{}, voice.Exchange{Out: newFakeOutput(), Tools: tools(nil)})
	done := sayAsync(sess, "First.")
	c.expect(t, "InjectAgentMessage")
	c.send(map[string]any{"type": "InjectionRefused", "message": "user is speaking"})
	if err := returned(t, done); !errors.Is(err, ErrRefused) {
		t.Fatalf("first Say = %v, want ErrRefused", err)
	}
	done = sayAsync(sess, "Second.")
	c.expect(t, "InjectAgentMessage")
	c.send(map[string]any{"type": "Warning", "code": "INJECT_AGENT_MESSAGE_DURING_USER_SPEECH", "description": "ignored"})
	notReturned(t, done, "on the second message of the first Say's refusal")
}

func sayAsync(sess voice.Session, text string) chan error {
	done := make(chan error, 1)
	go func() { done <- sess.Say(context.Background(), text, voice.Fact) }()
	return done
}

// notReturned fails if Say returns within 100 ms.
func notReturned(t *testing.T, done chan error, when string) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("Say returned %v %s", err, when)
	case <-time.After(100 * time.Millisecond):
	}
}

// returned waits for Say's result.
func returned(t *testing.T, done chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("Say did not return within 3s")
		return nil
	}
}

// TestInjectedSpeechPlaysThroughTheOutput fails if an injected answer's
// audio does not reach the Output as PCM16 at OutputRate, mono, in frames
// of at most 20 ms, byte for byte, between a SpeechStarted and a
// SpeechEnded once Deepgram says the audio is done and the playback drained.
func TestInjectedSpeechPlaysThroughTheOutput(t *testing.T) {
	setSpeechGap(t, time.Minute) // only AgentAudioDone ends the reply here
	s := startStandIn(t)
	out := newFakeOutput()
	sess, c, _ := open(t, s, Config{}, voice.Exchange{Out: out, Tools: tools(nil)})
	speech := tone(OutputRate, 70*time.Millisecond, 1234)
	done := sayAsync(sess, "It is in review.")
	c.expect(t, "InjectAgentMessage")
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "It is in review.", "conversational_behavior": "default"})
	c.sendAudio(speech[:1001]) // an odd split, as a network may deliver it
	c.sendAudio(speech[1001:])
	c.send(map[string]any{"type": "AgentAudioDone"})
	if err := returned(t, done); err != nil {
		t.Fatalf("Say = %v", err)
	}
	waitEvent(t, sess, voice.EventSpeechStarted)
	waitEvent(t, sess, voice.EventSpeechEnded)
	plays := out.playbacks()
	if len(plays) != 1 {
		t.Fatalf("%d playbacks began, want 1", len(plays))
	}
	p := plays[0]
	if got := p.written(); !bytes.Equal(got, speech) {
		t.Errorf("the Output got %d bytes, want the injection's %d bytes as sent", len(got), len(speech))
	}
	for _, f := range p.formats {
		if f != (audio.AudioFormat{SampleRate: OutputRate, Channels: 1, Encoding: audio.EncodingPCM16}) {
			t.Fatalf("a frame was written as %+v, want PCM16 mono at %d Hz", f, OutputRate)
		}
	}
	if limit := OutputRate * 2 * 20 / 1000; p.longest > limit {
		t.Errorf("the longest frame was %d bytes, want at most 20 ms (%d bytes)", p.longest, limit)
	}
	if !p.drained {
		t.Error("SpeechEnded came before the playback drained")
	}
}

// TestTheModelsOwnWordsBeforeAnyUserTurnAreNotPlayedOrReported fails if
// Deepgram's own reply reaches the Output or AgentText before the room has
// spoken. The real review probe heard 960 unauthorized bytes on a new
// session. Only an injected answer or routing line may play.
func TestTheModelsOwnWordsBeforeAnyUserTurnAreNotPlayedOrReported(t *testing.T) {
	setSpeechGap(t, time.Minute)
	s := startStandIn(t)
	out := newFakeOutput()
	sess, c, _ := open(t, s, Config{}, voice.Exchange{Out: out, Tools: tools(nil)})
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "Hello, I am listening.", "conversational_behavior": "default"})
	c.sendAudio(tone(OutputRate, 20*time.Millisecond, 100))
	c.send(map[string]any{"type": "AgentAudioDone"})
	select {
	case p := <-out.begun:
		t.Fatalf("the session began playback of %d unauthorized bytes before a user turn", len(p.written()))
	case e := <-sess.Events():
		if e.Kind == voice.EventAgentText {
			t.Fatalf("AgentText = %q before a user turn, want none", e.Text)
		}
	case <-time.After(300 * time.Millisecond):
	}
}

// TestTheModelsOwnWordsAfterOnlyAHeldAnswerAreNotPlayed fails if, after a
// session delivers only a held answer, Deepgram's next model reply reaches
// the Output or AgentText. The real review probe heard 960 unauthorized
// bytes on this path too. A held answer is an injection, not a licence for
// the model to speak after it.
func TestTheModelsOwnWordsAfterOnlyAHeldAnswerAreNotPlayed(t *testing.T) {
	setSpeechGap(t, time.Minute)
	s := startStandIn(t)
	out := newFakeOutput()
	sess, c, _ := open(t, s, Config{}, voice.Exchange{Out: out, Tools: tools(nil)})
	held := sayAsync(sess, "It ships today.")
	c.expect(t, "InjectAgentMessage")
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "It ships today.", "conversational_behavior": "default"})
	c.sendAudio(tone(OutputRate, 20*time.Millisecond, 100))
	c.send(map[string]any{"type": "AgentAudioDone"})
	if err := returned(t, held); err != nil {
		t.Fatalf("Say of the held answer = %v", err)
	}
	answer := out.nextPlayback(t)
	answer.waitBytes(t, 960)
	for {
		ev := waitEvent(t, sess, voice.EventAgentText)
		if ev.Text == "It ships today." {
			break
		}
		t.Fatalf("AgentText = %q while the held answer played, want its words", ev.Text)
	}
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "What else can I help with?", "conversational_behavior": "default"})
	c.sendAudio(tone(OutputRate, 20*time.Millisecond, 200))
	c.send(map[string]any{"type": "AgentAudioDone"})
	select {
	case p := <-out.begun:
		t.Fatalf("the session began playback of %d unauthorized bytes after the held answer", len(p.written()))
	case e := <-sess.Events():
		if e.Kind == voice.EventAgentText {
			t.Fatalf("AgentText = %q after the held answer, want none", e.Text)
		}
	case <-time.After(300 * time.Millisecond):
	}
}

// TestWordsNotPlayedAreNotReported fails if words the session does not play
// are reported as AgentText: the Supervisor's own-speech filter would then
// ignore room words that match words the room never heard.
func TestWordsNotPlayedAreNotReported(t *testing.T) {
	s := startStandIn(t)
	release := make(chan struct{})
	runner := tools(func(context.Context, voice.ToolCall) (json.RawMessage, error) {
		<-release
		return answerOf("Ada holds it.", ""), nil
	})
	sess, c, _ := open(t, s, Config{}, voice.Exchange{Out: newFakeOutput(), Tools: runner})
	c.send(map[string]any{"type": "ConversationText", "role": "user", "content": "who holds it"})
	c.send(answerCall("fc_l1", "who holds it"))
	waitEvent(t, sess, voice.EventToolCall)
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "The issue is held by Lucia.", "conversational_behavior": "default"})
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "Let me check.", "conversational_behavior": "filler"})
	close(release)
	// The muted reply is coming, so the call is answered before the answer
	// is injected; this test reads only what is reported.
	if f := c.nextText(t, 3*time.Second); f.typ == "FunctionCallResponse" {
		c.expect(t, "InjectAgentMessage")
	} else if f.typ != "InjectAgentMessage" {
		t.Fatalf("the session sent %s (%s), want the answer", f.typ, f.raw)
	}
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "Ada holds it."})
	for {
		ev := waitEvent(t, sess, voice.EventAgentText)
		if ev.Text == "Ada holds it." {
			return
		}
		t.Errorf("AgentText %q was reported, but those words did not play", ev.Text)
	}
}

// TestUserTurnIsReported fails if what Deepgram heard the user say is not
// reported as a UserTurn with its text.
func TestUserTurnIsReported(t *testing.T) {
	s := startStandIn(t)
	sess, c, _ := open(t, s, Config{}, voice.Exchange{Out: newFakeOutput(), Tools: tools(nil)})
	c.send(map[string]any{"type": "UserStartedSpeaking"})
	c.send(map[string]any{"type": "ConversationText", "role": "user", "content": "What is happening with the voice issue?", "conversational_behavior": "default"})
	if ev := waitEvent(t, sess, voice.EventUserTurn); ev.Text != "What is happening with the voice issue?" {
		t.Errorf("UserTurn = %q, want what the user said", ev.Text)
	}
}

// TestUserSpeechDoesNotStopTheVoice fails if Deepgram's UserStartedSpeaking
// stops the injected answer's playback: the Supervisor alone runs barge-in,
// by one rule for every voice.
func TestUserSpeechDoesNotStopTheVoice(t *testing.T) {
	setSpeechGap(t, time.Minute)
	s := startStandIn(t)
	out := newFakeOutput()
	out.blockWrites = true // the speaker is still playing it
	sess, c, _ := open(t, s, Config{}, voice.Exchange{Out: out, Tools: tools(nil)})
	done := sayAsync(sess, "It is in review.")
	c.expect(t, "InjectAgentMessage")
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "It is in review."})
	c.sendAudio(tone(OutputRate, 40*time.Millisecond, 100))
	out.nextPlayback(t)
	waitEvent(t, sess, voice.EventSpeechStarted)
	c.send(map[string]any{"type": "UserStartedSpeaking"})
	deadline := time.After(300 * time.Millisecond)
	for {
		select {
		case ev := <-sess.Events():
			if ev.Kind == voice.EventSpeechEnded {
				t.Fatal("UserStartedSpeaking stopped the voice's speech")
			}
		case <-deadline:
			_ = done
			return
		}
	}
}

// TestTheNextReplyAfterAReplyWithNoEndIsGated fails if, after a model reply
// that got no AgentAudioDone, the model's own reply to the room's next turn
// plays when its audio comes before its text.
func TestTheNextReplyAfterAReplyWithNoEndIsGated(t *testing.T) {
	setSpeechGap(t, time.Minute)
	s := startStandIn(t)
	out := newFakeOutput()
	runner := tools(func(context.Context, voice.ToolCall) (json.RawMessage, error) {
		return answerOf("Ada holds it.", ""), nil
	})
	_, c, _ := open(t, s, Config{}, voice.Exchange{Out: out, Tools: runner})
	c.send(map[string]any{"type": "UserStartedSpeaking"})
	c.sendAudio(tone(OutputRate, 20*time.Millisecond, 150)) // the model's reply, before its text
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "I am not sure."})
	c.send(map[string]any{"type": "ConversationText", "role": "user", "content": "who holds it"})
	c.expect(t, "InjectAgentMessage")
	answer := tone(OutputRate, 20*time.Millisecond, 300)
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "Ada holds it."})
	c.sendAudio(answer)
	p := out.nextPlayback(t)
	p.waitBytes(t, len(answer))
	if got := p.written(); !bytes.Equal(got, answer) {
		t.Fatalf("the speaker got %d bytes, want the answer's %d: the model's own reply played", len(got), len(answer))
	}
}

// TestAReplyBeforeTheTurnsWordsGoesToTheConcierge fails if the model's own
// reply that comes before the words of the turn it answers plays, or the
// turn goes unanswered.
func TestAReplyBeforeTheTurnsWordsGoesToTheConcierge(t *testing.T) {
	s := startStandIn(t)
	asked := make(chan voice.ToolCall, 1)
	runner := tools(func(_ context.Context, call voice.ToolCall) (json.RawMessage, error) {
		asked <- call
		return answerOf("Ada holds it.", ""), nil
	})
	_, c, _ := open(t, s, Config{}, voice.Exchange{Out: newFakeOutput(), Tools: runner})
	c.send(map[string]any{"type": "UserStartedSpeaking"})
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "I am not sure."})
	time.Sleep(50 * time.Millisecond)
	c.send(map[string]any{"type": "ConversationText", "role": "user", "content": "who holds it"})
	select {
	case call := <-asked:
		var args voice.AnswerArgs
		if err := json.Unmarshal(call.Args, &args); err != nil || args.Question != "who holds it" {
			t.Errorf("the Concierge was asked %s, want the turn's words", call.Args)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the turn the model answered before its words was not answered")
	}
	if msg := c.expect(t, "InjectAgentMessage"); msg["message"] != "Ada holds it." {
		t.Errorf("InjectAgentMessage = %v, want the Concierge's answer", msg)
	}
}

// TestAStaleAnswerDoesNotHoldUpTheNextTurn fails if an answer Deepgram never
// speaks keeps the next turn's answer waiting for the Say limit.
func TestAStaleAnswerDoesNotHoldUpTheNextTurn(t *testing.T) {
	s := startStandIn(t)
	answers := make(chan string, 2)
	answers <- "First answer."
	answers <- "Second answer."
	runner := tools(func(context.Context, voice.ToolCall) (json.RawMessage, error) {
		return answerOf(<-answers, ""), nil
	})
	_, c, _ := open(t, s, Config{}, voice.Exchange{Out: newFakeOutput(), Tools: runner})
	c.send(map[string]any{"type": "ConversationText", "role": "user", "content": "first question"})
	c.send(answerCall("fc_1", "first question"))
	c.expect(t, "InjectAgentMessage") // Deepgram never speaks it
	c.expect(t, "FunctionCallResponse")
	c.send(map[string]any{"type": "UserStartedSpeaking"})
	c.send(map[string]any{"type": "ConversationText", "role": "user", "content": "second question"})
	start := time.Now()
	c.send(answerCall("fc_2", "second question"))
	if msg := c.expect(t, "InjectAgentMessage"); msg["message"] != "Second answer." {
		t.Fatalf("the session sent %v, want the second answer", msg)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("the second answer waited %v behind the first, want under 1 s", took)
	}
}

// TestARefusedAnswerIsRetriedWithNoNewTurn fails if an answer Deepgram
// refused is lost when no turn of the room's follows.
func TestARefusedAnswerIsRetriedWithNoNewTurn(t *testing.T) {
	s := startStandIn(t)
	runner := tools(func(context.Context, voice.ToolCall) (json.RawMessage, error) {
		return answerOf("ABC-12 is in review.", ""), nil
	})
	_, c, _ := open(t, s, Config{retryGrace: 200 * time.Millisecond}, voice.Exchange{Out: newFakeOutput(), Tools: runner})
	c.send(map[string]any{"type": "ConversationText", "role": "user", "content": "what about ABC-12"})
	c.send(answerCall("fc_r", "what about ABC-12"))
	c.expect(t, "InjectAgentMessage")
	c.send(map[string]any{"type": "InjectionRefused"})
	c.expect(t, "FunctionCallResponse")
	if msg := c.expect(t, "InjectAgentMessage"); msg["message"] != "ABC-12 is in review." {
		t.Fatalf("with no new turn the session sent %v, want the refused answer again", msg)
	}
}

// TestSayWithNoAudioIsNotSpoken fails if a Say whose words Deepgram echoes
// but never sends audio for returns nil: the Supervisor would take an
// answer as heard that the room never heard.
func TestSayWithNoAudioIsNotSpoken(t *testing.T) {
	setSpeechGap(t, time.Minute)
	s := startStandIn(t)
	sess, c, _ := open(t, s, Config{sayLimit: 400 * time.Millisecond}, voice.Exchange{Out: newFakeOutput(), Tools: tools(nil)})
	done := sayAsync(sess, "A fact.")
	c.expect(t, "InjectAgentMessage")
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "A fact."})
	if err := returned(t, done); !errors.Is(err, ErrNotSpoken) {
		t.Fatalf("Say with no audio of its words = %v, want ErrNotSpoken", err)
	}
}

// TestSayWaitsForAudioThatComesLate fails if a Say returns before its
// words' audio, which Deepgram may send more than a second after their
// echo (its own reports show 1.16 s), is queued to the Output.
func TestSayWaitsForAudioThatComesLate(t *testing.T) {
	setSpeechGap(t, time.Minute)
	s := startStandIn(t)
	out := newFakeOutput()
	sess, c, _ := open(t, s, Config{}, voice.Exchange{Out: out, Tools: tools(nil)})
	done := sayAsync(sess, "A fact.")
	c.expect(t, "InjectAgentMessage")
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "A fact."})
	select {
	case err := <-done:
		t.Fatalf("Say returned %v before its words' audio came", err)
	case <-time.After(1300 * time.Millisecond):
	}
	speech := tone(OutputRate, 20*time.Millisecond, 100)
	c.sendAudio(speech)
	c.send(map[string]any{"type": "AgentAudioDone"})
	if err := returned(t, done); err != nil {
		t.Fatalf("Say: %v", err)
	}
	if got := out.nextPlayback(t).written(); !bytes.Equal(got, speech) {
		t.Fatalf("when Say returned the Output held %d bytes, want its words' %d", len(got), len(speech))
	}
}

// TestInterruptStopsTheAgentSpeech fails if, after Interrupt, the rest of
// an injected utterance Deepgram keeps sending is played, or the next
// injected utterance is not.
func TestInterruptStopsTheAgentSpeech(t *testing.T) {
	setSpeechGap(t, time.Minute)
	s := startStandIn(t)
	out := newFakeOutput()
	sess, c, _ := open(t, s, Config{}, voice.Exchange{Out: out, Tools: tools(nil)})
	first := tone(OutputRate, 40*time.Millisecond, 100)
	firstDone := injectSpeech(t, sess, c, "First.", first, false)
	p := out.nextPlayback(t)
	p.waitBytes(t, len(first))
	waitEvent(t, sess, voice.EventSpeechStarted)

	sess.Interrupt()
	_ = returned(t, firstDone)
	waitEvent(t, sess, voice.EventSpeechEnded)
	c.sendAudio(tone(OutputRate, 40*time.Millisecond, 300)) // rest of the interrupted utterance
	c.send(map[string]any{"type": "AgentAudioDone"})
	next := tone(OutputRate, 20*time.Millisecond, 400)
	nextDone := injectSpeech(t, sess, c, "Next.", next, true)
	p2 := out.nextPlayback(t)
	p2.waitBytes(t, len(next))
	if err := returned(t, nextDone); err != nil {
		t.Fatalf("Say of next = %v", err)
	}
	if got := p.written(); !bytes.Equal(got, first) {
		t.Errorf("the interrupted playback got %d bytes, want only the %d before Interrupt", len(got), len(first))
	}
	if got := p2.written(); !bytes.Equal(got, next) {
		t.Errorf("the next utterance played %d bytes, want its %d", len(got), len(next))
	}
}

// TestRoomAudioIsSilencedWhileTheVoiceSpeaks fails if room audio reaches
// Deepgram while an injected answer plays (so Deepgram's own barge-in cannot
// stop it), or if, after an Interrupt, the words said over the voice in the
// last 2 s are not sent on so the agent hears them.
func TestRoomAudioIsSilencedWhileTheVoiceSpeaks(t *testing.T) {
	setSpeechGap(t, time.Minute)
	s := startStandIn(t)
	out := newFakeOutput()
	tap := make(chan audio.Frame, 16)
	sess, c, _ := open(t, s, Config{}, voice.Exchange{Out: out, Tap: tap, Tools: tools(nil)})
	frame := func(v int16) audio.Frame {
		pcm := make([]int16, 160)
		for i := range pcm {
			pcm[i] = v
		}
		return audio.Frame{PCM: pcm, Rate: 16000, Channels: 1}
	}
	tap <- frame(7)
	if got := c.nextAudio(t); !bytes.Equal(got, pcm16(frame(7).PCM...)) {
		t.Fatal("room audio did not reach Deepgram while the voice was quiet")
	}
	done := injectSpeech(t, sess, c, "Voice.", tone(OutputRate, 20*time.Millisecond, 100), false)
	waitEvent(t, sess, voice.EventSpeechStarted)
	tap <- frame(9)
	if got := c.nextAudio(t); !bytes.Equal(got, make([]byte, 320)) {
		t.Fatalf("while the voice spoke Deepgram got %v..., want silence of the same length", got[:4])
	}
	sess.Interrupt()
	_ = returned(t, done)
	tap <- frame(11)
	if got := c.nextAudio(t); !bytes.Equal(got, pcm16(frame(9).PCM...)) {
		t.Fatalf("after Interrupt Deepgram got %v..., want the words said over the voice first", got[:4])
	}
	if got := c.nextAudio(t); !bytes.Equal(got, pcm16(frame(11).PCM...)) {
		t.Fatalf("after the held words Deepgram got %v..., want the live audio", got[:4])
	}
}

// TestInterruptSendsOnlyTheLast2s fails if, after the voice spoke over more
// than 2 s of room audio, an Interrupt sends Deepgram anything but the last
// 2 s of it, oldest first.
func TestInterruptSendsOnlyTheLast2s(t *testing.T) {
	setSpeechGap(t, time.Minute)
	s := startStandIn(t)
	out := newFakeOutput()
	tap := make(chan audio.Frame, 64)
	sess, c, _ := open(t, s, Config{}, voice.Exchange{Out: out, Tap: tap, Tools: tools(nil)})
	done := injectSpeech(t, sess, c, "Voice.", tone(OutputRate, 20*time.Millisecond, 100), false)
	waitEvent(t, sess, voice.EventSpeechStarted)
	// 2.6 s of room audio in 0.3 s buffers, each a sample count of its own.
	var room []int16
	for i := range 9 {
		pcm := make([]int16, 4800)
		for j := range pcm {
			pcm[j] = int16(i*10000 + j)
		}
		room = append(room, pcm...)
		tap <- audio.Frame{PCM: pcm, Rate: 16000, Channels: 1}
	}
	sent := 0
	for sent < 9 {
		f, ok := c.next(t, 3*time.Second)
		if !ok {
			t.Fatalf("Deepgram got %d silent frames, want 9", sent)
		}
		if f.binary {
			sent++
		}
	}
	sess.Interrupt()
	_ = returned(t, done)
	tap <- audio.Frame{PCM: []int16{1}, Rate: 16000, Channels: 1}
	var flushed []byte
	for {
		f, ok := c.next(t, 3*time.Second)
		if !ok {
			t.Fatal("no audio reached Deepgram after Interrupt")
		}
		if !f.binary {
			continue
		}
		if bytes.Equal(f.raw, pcm16(1)) {
			break
		}
		flushed = append(flushed, f.raw...)
	}
	if want := pcm16(room[len(room)-2*16000:]...); !bytes.Equal(flushed, want) {
		t.Fatalf("after Interrupt Deepgram got %d bytes of held audio, want the last 2 s (%d bytes) in order", len(flushed), len(want))
	}
}

// TestHeldRoomAudioIsDroppedWhenTheVoiceEnds fails if the room audio held
// while an injected answer spoke is sent after an utterance that nobody
// interrupted.
func TestHeldRoomAudioIsDroppedWhenTheVoiceEnds(t *testing.T) {
	setSpeechGap(t, time.Minute)
	s := startStandIn(t)
	out := newFakeOutput()
	tap := make(chan audio.Frame, 16)
	cfg := Config{echoTail: -1} // the room after the reply is heard at once
	sess, c, _ := open(t, s, cfg, voice.Exchange{Out: out, Tap: tap, Tools: tools(nil)})
	done := injectSpeech(t, sess, c, "Voice.", tone(OutputRate, 20*time.Millisecond, 100), false)
	waitEvent(t, sess, voice.EventSpeechStarted)
	tap <- audio.Frame{PCM: []int16{5, 5}, Rate: 16000, Channels: 1}
	if got := c.nextAudio(t); !bytes.Equal(got, make([]byte, 4)) {
		t.Fatalf("while the voice spoke Deepgram got %v, want silence", got)
	}
	c.send(map[string]any{"type": "AgentAudioDone"})
	if err := returned(t, done); err != nil {
		t.Fatalf("Say = %v", err)
	}
	waitEvent(t, sess, voice.EventSpeechEnded)
	tap <- audio.Frame{PCM: []int16{6, 6}, Rate: 16000, Channels: 1}
	if got := c.nextAudio(t); !bytes.Equal(got, pcm16(6, 6)) {
		t.Fatalf("after the voice ended Deepgram got %v, want the live audio and nothing held", got)
	}
}

// TestInterruptSendsTheHeldAudioBeforeTheNextReply fails if the words said
// over an interrupted injection wait unsent while a next injection plays
// that began before the next room buffer came: Deepgram would hear them
// only once that reply ended, with the room audio of a reply nobody
// interrupted.
func TestInterruptSendsTheHeldAudioBeforeTheNextReply(t *testing.T) {
	setSpeechGap(t, time.Minute)
	s := startStandIn(t)
	tap := make(chan audio.Frame, 16)
	sess, c, _ := open(t, s, Config{}, voice.Exchange{Out: newFakeOutput(), Tap: tap, Tools: tools(nil)})
	first := injectSpeech(t, sess, c, "First.", tone(OutputRate, 20*time.Millisecond, 100), false)
	waitEvent(t, sess, voice.EventSpeechStarted)
	tap <- audio.Frame{PCM: []int16{9, 9}, Rate: 16000, Channels: 1}
	if got := c.nextAudio(t); !bytes.Equal(got, make([]byte, 4)) {
		t.Fatalf("while the voice spoke Deepgram got %v, want silence", got)
	}
	sess.Interrupt()
	_ = returned(t, first)
	waitEvent(t, sess, voice.EventSpeechEnded)
	c.send(map[string]any{"type": "AgentAudioDone"}) // end of the interrupted reply
	second := injectSpeech(t, sess, c, "Second.", tone(OutputRate, 20*time.Millisecond, 200), false)
	waitEvent(t, sess, voice.EventSpeechStarted)
	tap <- audio.Frame{PCM: []int16{11, 11}, Rate: 16000, Channels: 1}
	if got := c.nextAudio(t); !bytes.Equal(got, pcm16(9, 9)) {
		t.Fatalf("after Interrupt Deepgram got %v, want the words said over the interrupted reply", got)
	}
	if got := c.nextAudio(t); !bytes.Equal(got, make([]byte, 4)) {
		t.Fatalf("while the next reply played Deepgram got %v, want silence", got)
	}
	_ = second
}

// TestSpeechEndsWhenDeepgramStopsSending fails if an injected reply whose
// audio stops with no AgentAudioDone, as when Deepgram cut it for someone
// who spoke, keeps the voice speaking and the room silenced for good; or if
// the next reply after an interrupted one that Deepgram stopped is not
// played. Each reply's text comes first, so its audio is held after the
// text and plays once no audio has come for the speech gap.
func TestSpeechEndsWhenDeepgramStopsSending(t *testing.T) {
	setSpeechGap(t, 300*time.Millisecond)
	s := startStandIn(t)
	out := newFakeOutput()
	sess, c, _ := open(t, s, Config{}, voice.Exchange{Out: out, Tools: tools(nil)})
	sayOpen := func(text string, pcm []byte) chan error {
		done := sayAsync(sess, text)
		c.expect(t, "InjectAgentMessage")
		c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": text})
		c.sendAudio(pcm)
		return done
	}
	first := sayOpen("First.", tone(OutputRate, 20*time.Millisecond, 100))
	waitEvent(t, sess, voice.EventSpeechStarted)
	waitEvent(t, sess, voice.EventSpeechEnded)
	_ = returned(t, first)

	second := sayOpen("Second.", tone(OutputRate, 20*time.Millisecond, 200))
	waitEvent(t, sess, voice.EventSpeechStarted)
	sess.Interrupt()
	_ = returned(t, second)
	waitEvent(t, sess, voice.EventSpeechEnded)
	time.Sleep(500 * time.Millisecond) // Deepgram sent nothing more of it
	next := tone(OutputRate, 20*time.Millisecond, 300)
	nextDone := sayOpen("Next.", next)
	waitEvent(t, sess, voice.EventSpeechStarted)
	plays := out.playbacks()
	last := plays[len(plays)-1]
	last.waitBytes(t, len(next))
	if len(plays) != 3 || !bytes.Equal(last.written(), next) {
		t.Fatalf("%d playbacks, the last with %d bytes; want 3, the last the next reply's %d", len(plays), len(last.written()), len(next))
	}
	_ = nextDone
}

// TestDeepgramErrorIsReported fails if an Error Deepgram sends mid-session
// is not reported as an Err event naming its code.
func TestDeepgramErrorIsReported(t *testing.T) {
	s := startStandIn(t)
	sess, c, _ := open(t, s, Config{}, voice.Exchange{Out: newFakeOutput(), Tools: tools(nil)})
	c.send(map[string]any{"type": "Error", "code": "FAILED_TO_THINK", "description": "every think provider failed"})
	ev := waitEvent(t, sess, voice.EventErr)
	if ev.Err == nil || !strings.Contains(ev.Err.Error(), "FAILED_TO_THINK") {
		t.Errorf("Err event = %v, want one naming FAILED_TO_THINK", ev.Err)
	}
}

// TestDisconnectIsReported fails if a connection that drops is not
// reported as an Err event that keeps the connection's own error, or if the
// events do not close after Close.
func TestDisconnectIsReported(t *testing.T) {
	s := startStandIn(t)
	sess, c, _ := open(t, s, Config{}, voice.Exchange{Out: newFakeOutput(), Tools: tools(nil)})
	c.drop()
	ev := waitEvent(t, sess, voice.EventErr)
	if !errors.Is(ev.Err, ErrDisconnected) {
		t.Errorf("Err event = %v, want ErrDisconnected", ev.Err)
	}
	var closeErr *websocket.CloseError
	if !errors.As(ev.Err, &closeErr) {
		t.Errorf("Err event = %v, want it to keep the connection's *websocket.CloseError", ev.Err)
	}
	_ = sess.Close()
	waitEventsClosed(t, sess)
}

// TestDeepgramDescriptionsStayOutOfTheLog fails if the free text of
// Deepgram's errors and warnings or an answer tool's error reaches the log
// at any level or the Err event: vendors and the Concierge can quote the
// room's words, which no delete path reaches in a log.
func TestDeepgramDescriptionsStayOutOfTheLog(t *testing.T) {
	const words = "ROOM-WORDS-QUOTED"
	s := startStandIn(t)
	var logs lockedBuf
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	runner := tools(func(context.Context, voice.ToolCall) (json.RawMessage, error) {
		return nil, errors.New("concierge: bad arguments near " + words)
	})
	sess, c, _ := open(t, s, Config{Logger: logger}, voice.Exchange{Out: newFakeOutput(), Tools: runner})
	c.send(map[string]any{"type": "Warning", "code": "FUNCTION_CALL_FAILED", "description": "call failed: " + words})
	c.send(answerCall("fc_w", "what about ABC-1"))
	c.expect(t, "FunctionCallResponse")
	c.send(map[string]any{"type": "Error", "code": "UNPARSABLE_CLIENT_MESSAGE", "description": "could not parse: " + words})
	ev := waitEvent(t, sess, voice.EventErr)
	if strings.Contains(ev.Err.Error(), words) {
		t.Errorf("Err event = %q, want it without Deepgram's description", ev.Err)
	}
	if !strings.Contains(ev.Err.Error(), "UNPARSABLE_CLIENT_MESSAGE") {
		t.Errorf("Err event = %q, want it to name the code", ev.Err)
	}
	_ = sess.Close()
	if strings.Contains(logs.String(), words) {
		t.Errorf("the log at Debug holds a description or tool error:\n%s", logs.String())
	}
	for _, code := range []string{"FUNCTION_CALL_FAILED", "UNPARSABLE_CLIENT_MESSAGE"} {
		if !strings.Contains(logs.String(), code) {
			t.Errorf("the log lacks the code %s:\n%s", code, logs.String())
		}
	}
}

func TestFallbackToolErrorStaysOutOfDebugLog(t *testing.T) {
	const words = "ROOM-WORDS-FALLBACK"
	s := startStandIn(t)
	var logs lockedBuf
	called := make(chan struct{}, 1)
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	runner := tools(func(context.Context, voice.ToolCall) (json.RawMessage, error) {
		called <- struct{}{}
		return nil, errors.New("fallback failure near " + words)
	})
	sess, c, _ := open(t, s, Config{Logger: logger}, voice.Exchange{Out: newFakeOutput(), Tools: runner})
	c.send(map[string]any{"type": "UserStartedSpeaking"})
	c.send(map[string]any{"type": "ConversationText", "role": "user", "content": "who holds it"})
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "I can help with that."})
	select {
	case <-called:
	case <-time.After(3 * time.Second):
		t.Fatal("the model's own answer did not reach the fallback tool")
	}
	// The failure is logged after the tool returns. Waiting for that line
	// keeps a Close from ending the run before the log this test reads.
	for deadline := time.Now().Add(3 * time.Second); !strings.Contains(logs.String(), "the answer failed"); {
		if time.Now().After(deadline) {
			t.Fatalf("UNKNOWN: the fallback failure was never logged:\n%s", logs.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	_ = sess.Close()
	if got := logs.String(); strings.Contains(got, words) {
		t.Fatalf("fallback tool error reached Debug log:\n%s", got)
	}
}

// TestCloseTearsDownWithin100ms fails if Close takes longer than 100 ms
// while a tool call runs and the Output is full, or leaves the connection
// open or the events unclosed.
func TestCloseTearsDownWithin100ms(t *testing.T) {
	setSpeechGap(t, time.Minute)
	s := startStandIn(t)
	out := newFakeOutput()
	out.blockWrites = true
	runner := tools(func(ctx context.Context, _ voice.ToolCall) (json.RawMessage, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	tap := make(chan audio.Frame)
	sess, c, _ := open(t, s, Config{}, voice.Exchange{Out: out, Tap: tap, Tools: runner})
	c.send(answerCall("fc_5", "x"))
	waitEvent(t, sess, voice.EventToolCall)
	// The Supervisor's routing line while the answer is found: the speech
	// that plays then.
	_ = sayAsync(sess, "Reading ABC-12.")
	c.expect(t, "InjectAgentMessage")
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "Reading ABC-12."})
	c.sendAudio(tone(OutputRate, 40*time.Millisecond, 100))
	out.nextPlayback(t)

	start := time.Now()
	if err := sess.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if took := time.Since(start); took > 100*time.Millisecond {
		t.Errorf("Close took %v, want at most 100 ms", took)
	}
	select {
	case <-c.ended:
	case <-time.After(time.Second):
		t.Error("the connection stayed open after Close")
	}
	waitEventsClosed(t, sess)
}

// TestCloseBeginsNoPlayback fails if Close, while an injected reply drains
// and the next injection waits in the queue, has the session begin a
// playback on the Output for speech it will never write.
func TestCloseBeginsNoPlayback(t *testing.T) {
	setSpeechGap(t, time.Minute)
	s := startStandIn(t)
	out := newFakeOutput()
	sess, c, _ := open(t, s, Config{}, voice.Exchange{Out: drainingOutput{out}, Tools: tools(nil)})
	first := tone(OutputRate, 20*time.Millisecond, 100)
	firstDone := sayAsync(sess, "First.")
	c.expect(t, "InjectAgentMessage")
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "First."})
	c.sendAudio(first)
	c.send(map[string]any{"type": "AgentAudioDone"})
	out.nextPlayback(t).waitBytes(t, len(first))
	if err := returned(t, firstDone); err != nil {
		t.Fatalf("Say of first = %v", err)
	}
	nextDone := sayAsync(sess, "Next.")
	c.expect(t, "InjectAgentMessage")
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "Next."})
	c.sendAudio(tone(OutputRate, 20*time.Millisecond, 200)) // waits behind the draining reply
	_ = sess.Close()
	if n := len(out.playbacks()); n != 1 {
		t.Fatalf("%d playbacks began, want only the one draining at Close", n)
	}
	_ = nextDone
}

// drainingOutput is an Output whose speech is still playing: a playback's
// Drain returns only once its context ends.
type drainingOutput struct{ *fakeOutput }

func (o drainingOutput) Begin() voice.Playback { return stillPlaying{o.fakeOutput.Begin()} }

type stillPlaying struct{ voice.Playback }

func (stillPlaying) Drain(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

// TestCloseIsQuickOnAStalledTLSConnection fails if Close, on a wss
// connection to a network that takes no more data, waits for TLS to send
// its close_notify, up to 5 s, rather than resetting the connection.
func TestCloseIsQuickOnAStalledTLSConnection(t *testing.T) {
	sess, raw := startTLS(t)
	raw.stall()
	began := time.Now()
	if err := sess.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if took := time.Since(began); took > 100*time.Millisecond {
		t.Errorf("Close took %v on a stalled connection, want at most 100 ms", took)
	}
}

// TestIdleSessionSendsKeepAlive fails if a session with no room audio to
// send lets Deepgram time it out (CLIENT_MESSAGE_TIMEOUT).
func TestIdleSessionSendsKeepAlive(t *testing.T) {
	s := startStandIn(t)
	_, c, _ := open(t, s, Config{keepAlive: 50 * time.Millisecond}, voice.Exchange{Out: newFakeOutput(), Tools: tools(nil)})
	deadline := time.After(2 * time.Second)
	for {
		select {
		case f := <-c.frames:
			if f.typ == "KeepAlive" {
				return
			}
		case <-deadline:
			t.Fatal("an idle session sent no KeepAlive")
		}
	}
}

// TestNewKeepsItsOwnConfig fails if a change the caller makes to its Config
// after New reaches the Settings a session sends.
func TestNewKeepsItsOwnConfig(t *testing.T) {
	const changed = "CHANGED-AFTER-NEW"
	s := startStandIn(t)
	cfg := Config{APIKey: standInKey, URL: s.url, Keyterms: []string{"Wispr"}}
	agent, err := New(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Keyterms[0] = changed
	done := make(chan error, 1)
	go func() {
		sess, err := agent.Open(context.Background(), &voice.Exchange{Out: newFakeOutput(), Tools: tools(nil)})
		if err == nil {
			err = sess.Close()
		}
		done <- err
	}()
	c := s.accept(t)
	settings := c.expect(t, "Settings")
	c.send(map[string]any{"type": "SettingsApplied"})
	if err := <-done; err != nil {
		t.Fatalf("Open: %v", err)
	}
	data, _ := json.Marshal(settings)
	if bytes.Contains(data, []byte(changed)) || !bytes.Contains(data, []byte(`"Wispr"`)) {
		t.Errorf("Settings = %s, want the keyterm as it was at New", data)
	}
}

// TestOpenRefusesAnExchangeWithNoTools fails if a session opens with no
// Concierge to run the functions it declares to Deepgram.
func TestOpenRefusesAnExchangeWithNoTools(t *testing.T) {
	s := startStandIn(t)
	agent, err := New(&Config{APIKey: "k", URL: s.url})
	if err != nil {
		t.Fatal(err)
	}
	// The stand-in plays the whole handshake, so only the session's own
	// check can refuse.
	go func() {
		select {
		case c := <-s.conns:
			<-c.frames // Settings
			c.send(map[string]any{"type": "SettingsApplied"})
		case <-time.After(3 * time.Second):
		}
	}()
	if sess, err := agent.Open(context.Background(), &voice.Exchange{Out: newFakeOutput()}); err == nil {
		_ = sess.Close()
		t.Fatal("Open with no tools succeeded")
	}
}

// TestSayWithNoWordsSendsNothing fails if Say sends an InjectAgentMessage
// with no words, on which Deepgram closes the session, or waits for an
// echo of them.
func TestSayWithNoWordsSendsNothing(t *testing.T) {
	s := startStandIn(t)
	sess, c, _ := open(t, s, Config{}, voice.Exchange{Out: newFakeOutput(), Tools: tools(nil)})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := sess.Say(ctx, " \n", voice.Fact); err != nil {
		t.Fatalf("Say with no words = %v, want nil at once", err)
	}
	c.quiet(t, 200*time.Millisecond, "InjectAgentMessage")
}

// TestDeliverRefusesAResultThatIsNotJSON fails if Deliver sends Deepgram a
// result that is not JSON, or uses up the call so the right one cannot
// follow.
func TestDeliverRefusesAResultThatIsNotJSON(t *testing.T) {
	s := startStandIn(t)
	runner := tools(func(ctx context.Context, _ voice.ToolCall) (json.RawMessage, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	sess, c, _ := open(t, s, Config{}, voice.Exchange{Out: newFakeOutput(), Tools: runner})
	c.send(answerCall("fc_6", "is it done"))
	waitEvent(t, sess, voice.EventToolCall)
	if err := sess.Deliver(context.Background(), "fc_6", json.RawMessage(`the holder said yes`)); err == nil {
		t.Error("Deliver of a result that is not JSON succeeded")
	}
	c.quiet(t, 200*time.Millisecond, "FunctionCallResponse")
	if err := sess.Deliver(context.Background(), "fc_6", answerOf("", "the holder said yes")); err != nil {
		t.Fatalf("Deliver after the refused result: %v", err)
	}
	if resp := c.expect(t, "FunctionCallResponse"); resp["id"] != "fc_6" || responseContent(t, resp)["answered"] != false {
		t.Errorf("FunctionCallResponse = %v, want fc_6, nothing handed over", resp)
	}
}

// TestOneAnswerPerTurn fails if more than one answer call runs for one user
// turn: the real probe showed Deepgram's own model call answer twice at
// once, and again after the answer it was told was not spoken, so the room
// heard answers twice and one it had talked over. Further calls are told at
// once, without running, that the turn is answered: told it was not, the
// model called again, up to nine times in three seconds (the proof).
func TestOneAnswerPerTurn(t *testing.T) {
	s := startStandIn(t)
	var runs atomic.Int32
	runner := tools(func(context.Context, voice.ToolCall) (json.RawMessage, error) {
		runs.Add(1)
		return answerOf("ABC-12 is in review.", ""), nil
	})
	_, c, _ := open(t, s, Config{}, voice.Exchange{Out: newFakeOutput(), Tools: runner})
	c.send(map[string]any{"type": "ConversationText", "role": "user", "content": "what about ABC-12"})
	c.send(answerCall("fc_a", ""))
	c.send(answerCall("fc_b", "what about ABC-12"))
	// The first call is spoken (its injection echoed as it comes); the
	// second is told at once that nothing was spoken.
	saw := map[string]map[string]any{}
	for saw["fc_a"] == nil || saw["fc_b"] == nil {
		f := c.nextText(t, 3*time.Second)
		var m map[string]any
		_ = json.Unmarshal(f.raw, &m)
		switch f.typ {
		case "InjectAgentMessage":
			if saw["inject"] != nil {
				t.Fatalf("a second injection for one turn: %s", f.raw)
			}
			saw["inject"] = m
			c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": m["message"]})
		case "FunctionCallResponse":
			saw[fmt.Sprint(m["id"])] = m
		}
	}
	if saw["fc_a"] == nil || responseContent(t, saw["fc_a"])["answered"] != true {
		t.Errorf("the turn's first answer call was answered %v, want answered", saw["fc_a"])
	}
	if saw["fc_b"] == nil || responseContent(t, saw["fc_b"])["answered"] != true {
		t.Errorf("the turn's second answer call was answered %v, want answered: the turn has its answer", saw["fc_b"])
	}
	c.quiet(t, 200*time.Millisecond, "InjectAgentMessage")
	c.send(answerCall("fc_c", "what about ABC-12"))
	if resp := c.expect(t, "FunctionCallResponse"); resp["id"] != "fc_c" || responseContent(t, resp)["answered"] != true {
		t.Errorf("a call again in the same turn was answered %v, want answered", resp)
	}
	if n := runs.Load(); n != 1 {
		t.Fatalf("the Concierge ran %d times for one turn, want once", n)
	}
	c.send(map[string]any{"type": "ConversationText", "role": "user", "content": "who holds it"})
	c.send(answerCall("fc_d", "who holds it"))
	c.expect(t, "InjectAgentMessage")
	if n := runs.Load(); n != 2 {
		t.Fatalf("the Concierge ran %d times after the next turn, want twice", n)
	}
}

// TestACancelledCallFreesItsTurn fails if, after Deepgram cancels a turn's
// answer call (the person spoke on), the call it makes again for that turn
// does not run.
func TestACancelledCallFreesItsTurn(t *testing.T) {
	s := startStandIn(t)
	var runs atomic.Int32
	runner := tools(func(ctx context.Context, _ voice.ToolCall) (json.RawMessage, error) {
		if runs.Add(1) == 1 {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return answerOf("ABC-12 is in review.", ""), nil
	})
	sess, c, _ := open(t, s, Config{}, voice.Exchange{Out: newFakeOutput(), Tools: runner})
	c.send(map[string]any{"type": "ConversationText", "role": "user", "content": "what about ABC-12"})
	c.send(answerCall("fc_1", "what about"))
	waitEvent(t, sess, voice.EventToolCall)
	c.send(map[string]any{"type": "FunctionCallCancelled", "functions": []any{map[string]any{"id": "fc_1", "name": voice.AnswerTool}}})
	c.send(answerCall("fc_2", "what about ABC-12"))
	if msg := c.expect(t, "InjectAgentMessage"); msg["message"] != "ABC-12 is in review." {
		t.Fatalf("after the cancelled call, the session sent %v, want the second call's answer", msg)
	}
}

// TestTheModelsOwnWordsAfterAnAnswerAreNotPlayed fails if words the model
// adds after a spoken answer reach the speaker: the model keeps the turn
// and speaks no fact, and the real probe heard gpt-4o-mini add one.
func TestTheModelsOwnWordsAfterAnAnswerAreNotPlayed(t *testing.T) {
	setSpeechGap(t, time.Minute)
	s := startStandIn(t)
	out := newFakeOutput()
	const say = "ABC-12 is in review."
	runner := tools(func(context.Context, voice.ToolCall) (json.RawMessage, error) { return answerOf(say, ""), nil })
	sess, c, _ := open(t, s, Config{}, voice.Exchange{Out: out, Tools: runner})
	c.send(map[string]any{"type": "ConversationText", "role": "user", "content": "what about ABC-12"})
	c.send(answerCall("fc_m", "what about ABC-12"))
	c.expect(t, "InjectAgentMessage")
	answer := tone(OutputRate, 40*time.Millisecond, 100)
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": say, "conversational_behavior": "default"})
	c.sendAudio(answer)
	c.expect(t, "FunctionCallResponse")
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "It was last changed 40 minutes ago.", "conversational_behavior": "default"})
	c.sendAudio(tone(OutputRate, 40*time.Millisecond, 200))
	c.send(map[string]any{"type": "AgentAudioDone"})
	waitEvent(t, sess, voice.EventSpeechEnded)
	if got := out.nextPlayback(t).written(); !bytes.Equal(got, answer) {
		t.Errorf("the speaker got %d bytes, want only the answer's %d", len(got), len(answer))
	}
	// The room's next turn is the model's to take again: its own reply to
	// it goes to the Concierge.
	c.send(map[string]any{"type": "ConversationText", "role": "user", "content": "and who holds it?"})
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "I am not sure.", "conversational_behavior": "default"})
	if msg := c.expect(t, "InjectAgentMessage"); msg["message"] != say {
		t.Errorf("after the next turn the session sent %v, want the Concierge's answer to it", msg)
	}
}

// TestACallBeforeItsTurnsWordsAnswersThatTurn fails if, when Deepgram
// sends the answer call before the words of the turn it answers (the real
// probe saw both orders), the turn is counted twice: the model's own words
// after the answer would then ask the Concierge again.
func TestACallBeforeItsTurnsWordsAnswersThatTurn(t *testing.T) {
	setSpeechGap(t, time.Minute)
	s := startStandIn(t)
	var runs atomic.Int32
	const say = "ABC-12 is in review."
	runner := tools(func(context.Context, voice.ToolCall) (json.RawMessage, error) {
		runs.Add(1)
		return answerOf(say, ""), nil
	})
	_, c, _ := open(t, s, Config{}, voice.Exchange{Out: newFakeOutput(), Tools: runner})
	c.send(map[string]any{"type": "UserStartedSpeaking"})
	c.send(answerCall("fc_e", "what about ABC-12"))
	c.send(map[string]any{"type": "ConversationText", "role": "user", "content": "what about ABC-12"})
	c.expect(t, "InjectAgentMessage")
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": say})
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "It is in review.", "conversational_behavior": "default"})
	c.quiet(t, 300*time.Millisecond, "InjectAgentMessage")
	if n := runs.Load(); n != 1 {
		t.Fatalf("the Concierge ran %d times for one turn, want once", n)
	}
}

// TestAReplyOfTheModelsOwnGoesToTheConcierge fails if the model, answering
// a turn of the room's with its own words and no answer call, is heard, or
// if that turn is not answered by the Concierge, word for word. The real
// probe heard phonellm answer a follow-up with "I don't have that detail
// in the records." instead of calling answer.
func TestAReplyOfTheModelsOwnGoesToTheConcierge(t *testing.T) {
	setSpeechGap(t, time.Minute)
	s := startStandIn(t)
	out := newFakeOutput()
	asked := make(chan voice.ToolCall, 2)
	runner := tools(func(_ context.Context, call voice.ToolCall) (json.RawMessage, error) {
		asked <- call
		return answerOf("Ada holds it.", ""), nil
	})
	sess, c, _ := open(t, s, Config{}, voice.Exchange{Out: out, Tools: runner})
	c.send(map[string]any{"type": "UserStartedSpeaking"})
	c.send(map[string]any{"type": "ConversationText", "role": "user", "content": "Wait, who holds that issue?"})
	c.sendAudio(tone(OutputRate, 20*time.Millisecond, 100)) // before its text, as Deepgram may send it
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "I don't have that detail in the records.", "conversational_behavior": "default"})
	c.sendAudio(tone(OutputRate, 20*time.Millisecond, 100))
	c.send(map[string]any{"type": "AgentAudioDone"})
	if ev := waitEvent(t, sess, voice.EventToolCall); ev.Call.Name != voice.AnswerTool {
		t.Errorf("ToolCall event = %+v, want the answer tool", ev.Call)
	}
	select {
	case call := <-asked:
		var args voice.AnswerArgs
		if err := json.Unmarshal(call.Args, &args); err != nil || args.Question != "Wait, who holds that issue?" {
			t.Errorf("the Concierge was asked %s, want the turn's words", call.Args)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the Concierge was not asked the turn the model answered for itself")
	}
	if msg := c.expect(t, "InjectAgentMessage"); msg["message"] != "Ada holds it." {
		t.Errorf("InjectAgentMessage = %v, want the Concierge's answer", msg)
	}
	c.quiet(t, 100*time.Millisecond, "FunctionCallResponse") // no call of Deepgram's to answer
	answer := tone(OutputRate, 20*time.Millisecond, 300)
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "Ada holds it."})
	c.sendAudio(answer)
	p := out.nextPlayback(t)
	p.waitBytes(t, len(answer))
	if got := p.written(); !bytes.Equal(got, answer) {
		t.Fatalf("the speaker got %d bytes first, want the Concierge's answer only: the model's own words played", len(got))
	}
	// The model's answer call for the same turn, if it comes, runs nothing.
	c.send(answerCall("fc_late", "who holds that issue"))
	if resp := c.expect(t, "FunctionCallResponse"); responseContent(t, resp)["answered"] != true {
		t.Errorf("the model's call after the Concierge answered its turn = %v, want answered, without running", resp)
	}
	select {
	case call := <-asked:
		t.Errorf("the Concierge was asked again: %s", call.Args)
	default:
	}
}

// TestFunctionCallResponseCarriesTheThoughtSignature fails if the answer to
// a call that came with a thought_signature, as some Gemini models send,
// does not carry it back unchanged, or if an answer to a call without one
// carries the field.
func TestFunctionCallResponseCarriesTheThoughtSignature(t *testing.T) {
	s := startStandIn(t)
	runner := tools(func(context.Context, voice.ToolCall) (json.RawMessage, error) {
		return answerOf("", "no agents"), nil
	})
	_, c, _ := open(t, s, Config{}, voice.Exchange{Out: newFakeOutput(), Tools: runner})
	c.send(map[string]any{"type": "FunctionCallRequest", "functions": []any{
		map[string]any{"id": "fc_7", "name": voice.AnswerTool, "arguments": `{"question":"who joined"}`, "client_side": true, "thought_signature": "sig-7"},
	}})
	if resp := c.expect(t, "FunctionCallResponse"); resp["id"] != "fc_7" || resp["thought_signature"] != "sig-7" {
		t.Errorf("FunctionCallResponse = %v, want fc_7 with thought_signature sig-7", resp)
	}
	c.send(map[string]any{"type": "FunctionCallRequest", "functions": []any{
		map[string]any{"id": "fc_8", "name": voice.AnswerTool, "arguments": `{"question":"who joined"}`, "client_side": true},
	}})
	if resp := c.expect(t, "FunctionCallResponse"); resp["id"] != "fc_8" || resp["thought_signature"] != nil {
		t.Errorf("FunctionCallResponse = %v, want fc_8 with no thought_signature", resp)
	}
}
