package dgagent

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// The Settings message (developers.deepgram.com/docs/configure-voice-agent).
// It carries no Think Live (/docs/voice-agent-think-live): voice C uses
// none of its fillers, backchannels, presence checks or frustration
// events, and its user backchannel detection can hold the room's speech
// during the agent's speech for a backchannel. The end-to-end proof at
// 66b984b heard a follow-up question go unheard in 5 of 5 runs while
// Deepgram streamed the model's own muted reply.
type settings struct {
	Type      string        `json:"type"`
	MIPOptOut bool          `json:"mip_opt_out"`
	Audio     audioSettings `json:"audio"`
	Agent     agentSettings `json:"agent"`
}

type audioSettings struct {
	Input  audioFormat `json:"input"`
	Output audioFormat `json:"output"`
}

type audioFormat struct {
	Encoding   string `json:"encoding"`
	SampleRate int    `json:"sample_rate"`
	Container  string `json:"container,omitempty"`
}

type agentSettings struct {
	Listen listenSettings  `json:"listen"`
	Think  []thinkSettings `json:"think"` // in order: Deepgram falls back to the next
	Speak  speakSettings   `json:"speak"`
}

type listenSettings struct {
	Provider listenProvider `json:"provider"`
}

type listenProvider struct {
	Type     string   `json:"type"`
	Version  string   `json:"version"`
	Model    string   `json:"model"`
	Keyterms []string `json:"keyterms,omitempty"`
}

type thinkSettings struct {
	Provider  thinkProvider   `json:"provider"`
	Prompt    string          `json:"prompt"`
	Functions []functionEntry `json:"functions"`
}

type thinkProvider struct {
	Type  string `json:"type"`
	Model string `json:"model"`
}

// functionEntry is one client-side function: it has no endpoint, so
// Deepgram sends its calls to the session.
type functionEntry struct {
	Name          string          `json:"name"`
	Description   string          `json:"description"`
	Parameters    json.RawMessage `json:"parameters"`
	DeferUntilEOT bool            `json:"defer_until_eot,omitempty"`
}

type speakSettings struct {
	Provider speakProvider `json:"provider"`
}

type speakProvider struct {
	Type    string `json:"type"`
	Version string `json:"version"`
	Model   string `json:"model"`
}

// answerFunction is the one function the voice declares: the contract's
// answer tool, which hands the asker's question to the Concierge. It is held
// to the end of the turn because the Concierge may send a question to an
// agent, which cannot be taken back, and Deepgram otherwise calls before the
// turn is confirmed, while a person may still be speaking.
var answerFunction = functionEntry{
	Name: voice.AnswerTool,
	Description: "Answer what the person asked Wispr, from the team's records. " +
		"The answer is spoken to the room word for word for you.",
	Parameters: json.RawMessage(`{"type":"object","properties":{"question":{"type":"string",` +
		`"description":"What the person asked Wispr, in their own words"}},"required":["question"]}`),
	DeferUntilEOT: true,
}

func (a *Agent) settings(x *voice.Exchange) settings {
	instructions := prompt(a.cfg.Prompt, x)
	think := make([]thinkSettings, len(a.think))
	for i, m := range a.think {
		think[i] = thinkSettings{
			Provider:  thinkProvider{Type: m.provider, Model: m.model},
			Prompt:    instructions,
			Functions: []functionEntry{answerFunction},
		}
	}
	return settings{
		Type:      "Settings",
		MIPOptOut: true,
		Audio: audioSettings{
			Input:  audioFormat{Encoding: "linear16", SampleRate: InputRate},
			Output: audioFormat{Encoding: "linear16", SampleRate: OutputRate, Container: "none"},
		},
		Agent: agentSettings{
			Listen: listenSettings{Provider: listenProvider{
				Type: "deepgram", Version: "v2", Model: "flux-general-en", Keyterms: a.cfg.Keyterms,
			}},
			Think: think,
			Speak: speakSettings{Provider: speakProvider{Type: "deepgram", Version: "v1", Model: a.cfg.SpeakModel}},
		},
	}
}

// DefaultPrompt is the think prompt's standing part: the model keeps the
// turn and hands every question to the answer function, which speaks the
// Concierge's text word for word, so the model itself states no fact.
const DefaultPrompt = `You are Wispr, the voice of a team's software agents, on a speakerphone in an office meeting. People in the room call you by name to ask about the team's work: what is happening with an issue, who holds it, or what its agent says.

When someone asks you something, including a follow-up to an answer, call the answer function at once, with their question in their own words. You have no records of your own: the answer function finds the answer in the team's records and has it spoken to the room word for word. It returns {"answered": true} once the answer is being spoken, or {"answered": false} when there is none to speak. Either way, say nothing more: do not repeat, reword or add to the answer, and do not ask if there is anything else.

Never answer from your own knowledge, and never say anything yourself. Only a question or request said to you gets a call. After an answer, people often go back to their meeting: words that are not a question to you, such as thanks, agreement, or moving on to another topic, get no call and no words. Do not turn them into a question. When people talk to each other, stay silent.`

// prompt is the think prompt: the standing part, then what the room said
// before the voice opened, so the model hears the question in context.
func prompt(base string, x *voice.Exchange) string {
	var b strings.Builder
	b.WriteString(base)
	if len(x.Recent) > 0 {
		b.WriteString("\n\nWhat the room said in the last two minutes, for context (it was not said to you):\n")
		for _, turn := range x.Recent {
			if turn.Speaker < 0 {
				b.WriteString("Unknown speaker: ")
			} else {
				fmt.Fprintf(&b, "Speaker %d: ", turn.Speaker)
			}
			b.WriteString(turn.Text)
			b.WriteByte('\n')
		}
	}
	if x.AskerTurn != "" {
		fmt.Fprintf(&b, "\nThe room's transcript heard the person calling you say: %q. You hear the same words; this text spells names and issue keys as the transcript did.", x.AskerTurn)
	}
	return b.String()
}
