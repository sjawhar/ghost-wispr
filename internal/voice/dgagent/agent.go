// Package dgagent is voice C: Deepgram's Voice Agent hears the room, keeps
// the turn with a model Deepgram hosts, and speaks with Aura-2, while every
// answer comes from the Concierge on the unit and is spoken word for word.
// Deepgram calls nothing on the unit, which has no public address: the one
// function it may call, answer, is declared client-side, so its calls come
// back down the session's own WebSocket.
//
// The session owns its WebSocket, as cmd/ghost-wispr's live transcription
// does, because the deepgram-go-sdk's InjectAgentMessage puts the text in a
// field the API does not read.
package dgagent

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"time"

	"github.com/dvonthenen/websocket"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/logging"
	"github.com/sjawhar/ghost-wispr/internal/netdial"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

const (
	// DefaultURL is Deepgram's Voice Agent endpoint.
	DefaultURL = "wss://agent.deepgram.com/v1/agent/converse"
	// InputRate is the room audio's rate: the tap gives 16 kHz mono.
	InputRate = 16000
	// OutputRate is the rate of the voice's speech, PCM16 mono, as the
	// session declares it to Deepgram and writes it to the Output.
	OutputRate = 24000
	// DefaultSpeakModel is the Aura-2 voice.
	DefaultSpeakModel = "aura-2-thalia-en"
)

var (
	// ErrDisconnected is the Err event of a session whose connection ended
	// before Close.
	ErrDisconnected = errors.New("deepgram voice agent disconnected")
	// ErrRefused is what Say returns when Deepgram will not speak the words
	// now, because someone holds the floor.
	ErrRefused = errors.New("deepgram refused to speak the words now")
	// ErrNoPendingCall is what Deliver returns for a call that is not
	// waiting for a result.
	ErrNoPendingCall = errors.New("no function call waits for that id")
	// ErrClosed is what a session's calls return after Close.
	ErrClosed = errors.New("voice session closed")
	// ErrInterrupted is what Say returns when Interrupt stopped its words.
	ErrInterrupted = errors.New("the voice was interrupted")
	// ErrNotSpoken is what Say returns when Deepgram did not speak the words
	// within the session's limit.
	ErrNotSpoken = errors.New("deepgram did not speak the words in time")
)

// DefaultThinkModel is the model that keeps the voice's turns, as
// provider/model, and fallbackThinkModel stands behind whichever model the
// setting names: Deepgram moves to it when the first fails a turn
// (/docs/voice-agent-llm-models, "Using multiple LLM providers"). The model
// only hears the question and calls answer; the Concierge words the answer.
//
// Deepgram's own phonellm-mini-alpha-2-fp8 is first: it was the fastest in
// every probe run, 1.19 s to 2.15 s from the end of the question to the
// first audio against 2.12 s to 2.74 s for gpt-4.1-mini (probe_test.go,
// 2026-11-19), and unlike Deepgram's think-fast it takes mip_opt_out. It is
// in Early Access, so a model Deepgram runs for its customers stands behind
// it.
const (
	DefaultThinkModel  = "deepgram/phonellm-mini-alpha-2-fp8"
	fallbackThinkModel = "open_ai/gpt-4.1-mini"
)

// refusalEcho is how soon after Deepgram refuses an injection its other
// refusal message for the same injection may come: the Inject Agent page
// names InjectionRefused, the Think Live page the INJECT_AGENT_MESSAGE_
// warning, and a session may get both.
const refusalEcho = 250 * time.Millisecond

// thinkModel is one model that keeps the voice's turns, one Deepgram hosts
// itself or runs for its customers: the unit has no public address, so it
// cannot serve a model of its own to Deepgram.
type thinkModel struct{ provider, model string }

// parseThinkModel reads a provider/model name with the config check's
// parser, so the two never disagree.
func parseThinkModel(name string) (thinkModel, error) {
	provider, model, err := config.ParseThinkModel(name)
	if err != nil {
		return thinkModel{}, fmt.Errorf("dgagent: %w", err)
	}
	return thinkModel{provider: provider, model: model}, nil
}

// handshakeTimeout bounds the dial and the Settings exchange.
const handshakeTimeout = 10 * time.Second

// writeTimeout bounds one write to the socket; a write that takes longer
// fails and ends the connection.
const writeTimeout = 5 * time.Second

// Config is voice C's configuration.
type Config struct {
	APIKey     string // the unit's Deepgram key
	URL        string // DefaultURL when empty
	ThinkModel string // provider/model; DefaultThinkModel when empty
	SpeakModel string // DefaultSpeakModel when empty
	Keyterms   []string
	Prompt     string // DefaultPrompt when empty
	Logger     *slog.Logger

	// Timings a test may shorten; New fills in the defaults.
	echoTail   time.Duration // the room is silenced this long after the voice's speech ends; <0 for none
	sayLimit   time.Duration // how long Say waits for Deepgram to speak its words
	ackLimit   time.Duration // how long Say waits for Deepgram to start an Ack, such as a routing line
	speechGap  time.Duration // a reply has ended when no more of it came for this long
	keepAlive  time.Duration // how often the session tells Deepgram it is still there
	retryGrace time.Duration // how long after a refusal or the room's turn a refused answer is handed over again
	askerWait  time.Duration // how long after the session opens with no turn heard the asker's words go to Deepgram as text
}

// Timings, as Config's defaults.
const (
	// echoTail is how long after the voice's speech ends the room is still
	// sent to Deepgram as silence: the mic's next 250 ms buffer was recorded
	// while the speaker still played the last words, and the speaker plays a
	// little after the Output takes the last frame. Deepgram would hear those
	// words as someone in the room, and answer them or cut the next reply.
	echoTail = 350 * time.Millisecond
	// sayLimit bounds Say's wait for Deepgram to start speaking its words:
	// they queue behind any reply of its own, which a long one could hold up
	// to half a minute. A Say past it returns ErrNotSpoken, so the
	// Supervisor keeps the answer rather than hold the floor for it.
	sayLimit = 30 * time.Second
	// ackLimit bounds Say's wait for Deepgram to start an Ack, such as the
	// routing line said while an answer call is pending. Deepgram starts an
	// injection within about 100 ms when it says nothing else, but holds it
	// behind the model's own reply to the call, which waits for the answer:
	// past this a routing line is late, and a Say that waits for it holds
	// the answer up (the end-to-end proof at 8144e62 saw it wait 30 s).
	ackLimit = 3 * time.Second
	// speechGap ends an utterance whose audio has all played when no more
	// comes for this long, in case Deepgram does not say the audio is done,
	// such as after it stopped the reply for someone who spoke. Deepgram
	// streams speech faster than it plays, so a gap this long means the
	// reply has stopped.
	speechGap = time.Second
	// keepAlive is how often the session tells Deepgram it is still there,
	// so a stretch with no room audio, such as a closed gate, does not end
	// it.
	keepAlive = 5 * time.Second
	// retryGrace is how long after the room's turn ends, or after a refusal
	// with no turn after it, an answer Deepgram refused because someone
	// spoke as it was handed over is handed over again, unless the turn
	// asked a new question: Deepgram calls answer for a question within
	// about a second of the turn's end.
	retryGrace = 1500 * time.Millisecond
	// turnWordsWait is how long a turn the model answered in its own words
	// waits for the turn's words, which Deepgram may send after the reply,
	// before the Concierge is asked it without them.
	turnWordsWait = 1500 * time.Millisecond
	// askerWait is how long after the session opens Deepgram may hear no
	// turn of the room's before the asker's words at the wake go to it as
	// text. The live transcript can lag the mic by more than the tap holds,
	// so the question's audio is gone before the session opens; otherwise
	// Deepgram hears the question's start within about a second.
	askerWait = 3 * time.Second
)

// Agent is voice C. It implements voice.Voice.
type Agent struct {
	cfg   Config
	think []thinkModel // in order: Deepgram falls back to the next
}

var (
	_ voice.Voice      = (*Agent)(nil)
	_ voice.TurnDriver = (*Agent)(nil)
)

// DrivesTurn says Deepgram drives voice C's turns: it hears the room, decides
// when the asker is done, and asks the Concierge for the answer itself.
func (*Agent) DrivesTurn() bool { return true }

// New returns voice C with cfg's defaults filled in. It keeps its own copy
// of cfg's lists, so a change the caller makes to cfg afterwards does not
// reach the voice.
func New(cfg *Config) (*Agent, error) {
	c := *cfg
	if c.APIKey == "" {
		return nil, errors.New("dgagent: no Deepgram API key")
	}
	if c.URL == "" {
		c.URL = DefaultURL
	}
	if c.ThinkModel == "" {
		c.ThinkModel = DefaultThinkModel
	}
	first, err := parseThinkModel(c.ThinkModel)
	if err != nil {
		return nil, err
	}
	think := []thinkModel{first}
	if fallback, _ := parseThinkModel(fallbackThinkModel); fallback != first {
		think = append(think, fallback)
	}
	c.Keyterms = slices.Clone(c.Keyterms)
	if c.SpeakModel == "" {
		c.SpeakModel = DefaultSpeakModel
	}
	if c.Prompt == "" {
		c.Prompt = DefaultPrompt
	}
	c.Logger = logging.WithModule(c.Logger, "dgagent")
	switch {
	case c.echoTail == 0:
		c.echoTail = echoTail
	case c.echoTail < 0:
		c.echoTail = 0
	}
	c.sayLimit = cmp.Or(c.sayLimit, sayLimit)
	c.ackLimit = cmp.Or(c.ackLimit, ackLimit)
	c.speechGap = cmp.Or(c.speechGap, speechGap)
	c.keepAlive = cmp.Or(c.keepAlive, keepAlive)
	c.retryGrace = cmp.Or(c.retryGrace, retryGrace)
	c.askerWait = cmp.Or(c.askerWait, askerWait)
	return &Agent{cfg: c, think: think}, nil
}

// Open connects a Voice Agent session for x, applies its Settings, and
// returns once Deepgram has applied them. ctx bounds the opening only; the
// session lasts until Close, and keeps what it needs of x.
func (a *Agent) Open(ctx context.Context, x *voice.Exchange) (voice.Session, error) {
	if x.Out == nil {
		return nil, errors.New("dgagent: the exchange has no Output to speak through")
	}
	if x.Tools == nil {
		return nil, errors.New("dgagent: the exchange has no tools to run the answer function")
	}
	ctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	header := http.Header{}
	header.Set("Authorization", "token "+a.cfg.APIKey)
	// ctx already bounds the handshake. TCP_USER_TIMEOUT ends the connection
	// once sent data stays unacknowledged as long as a write may take, as
	// the unit's other Deepgram sockets do.
	tcp := &net.Dialer{Control: netdial.TCPUserTimeout(writeTimeout)}
	dialer := websocket.Dialer{NetDialContext: tcp.DialContext}
	conn, resp, err := dialer.DialContext(ctx, a.cfg.URL, header)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("dgagent: connect: Deepgram answered %s: %w", resp.Status, err)
		}
		return nil, fmt.Errorf("dgagent: connect: %w", err)
	}
	// A read blocked on the handshake returns when ctx ends.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	err = a.handshake(conn, x)
	if fired := !stop(); fired || err != nil {
		_ = conn.Close()
		if fired {
			// The read failed because ctx ended: say so, not only how.
			err = errors.Join(ctx.Err(), err)
		}
		return nil, fmt.Errorf("dgagent: open: %w", err)
	}
	return start(&a.cfg, conn, x), nil
}

// handshake waits for Welcome, sends Settings and waits for SettingsApplied,
// the order the docs require (/docs/voice-agent-message-flow).
func (a *Agent) handshake(conn *websocket.Conn, x *voice.Exchange) error {
	if err := await(conn, "Welcome"); err != nil {
		return err
	}
	data, err := json.Marshal(a.settings(x))
	if err != nil {
		return err
	}
	_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		return fmt.Errorf("send Settings: %w", err)
	}
	return await(conn, "SettingsApplied")
}

// await reads until a message of type want, failing on Deepgram's Error.
func await(conn *websocket.Conn, want string) error {
	for {
		kind, data, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("waiting for %s: %w", want, withoutCloseText(err))
		}
		if kind != websocket.TextMessage {
			continue
		}
		var m serverMessage
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}
		switch m.Type {
		case want:
			return nil
		case "Error":
			return fmt.Errorf("waiting for %s: %w", want, m.err())
		}
	}
}

// withoutCloseText is err with the text of a close frame Deepgram sent
// taken out, keeping its code: the text is Deepgram's free text, which may
// quote what the session sent, and the room's words are in that.
func withoutCloseText(err error) error {
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) {
		return &websocket.CloseError{Code: closeErr.Code}
	}
	return err
}
