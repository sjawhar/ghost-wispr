package concierge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	dgws "github.com/dvonthenen/websocket"
	"github.com/gorilla/websocket"

	"github.com/sjawhar/ghost-wispr/internal/voice"
	"github.com/sjawhar/ghost-wispr/internal/voice/dgagent"
	"github.com/sjawhar/ghost-wispr/internal/voice/gptlive"
)

// Sol's fourteenth review of #191 at 2683f57
// (https://github.com/sjawhar/ghost-wispr/pull/191#pullrequestreview-6068248007),
// from Sol's probes, through the real Supervisor, Concierge and vendor
// clients, with a loopback vendor socket.

// openingRig is a real Supervisor and Concierge over v, the voice under
// test, with the Concierge's take-back rule.
func openingRig(t *testing.T, v voice.Voice) *cutRig {
	t.Helper()
	r := &cutRig{rig: newRig(t), speaker: &cutSpeaker{}}
	t.Cleanup(r.concierge.Stop)
	r.sup = voice.NewSupervisor(&voice.SupervisorConfig{
		Voice:        func() (voice.Voice, error) { return v, nil },
		Concierge:    r.concierge,
		Speaker:      r.speaker,
		WakePhrases:  []string{"Wispr"},
		TakeBack:     TakeBack,
		Red:          func() bool { return true },
		Session:      func() string { return "meeting-1" },
		IdleWait:     time.Minute,
		FollowUpWait: time.Minute,
		Logf:         t.Logf,
	})
	t.Cleanup(r.sup.Close)
	return r
}

// openingWords is the room's opening call, with a take-back when taken.
func openingWords(taken bool) string {
	if taken {
		return "Wispr, what about ABC-42? never mind. what about ABC-12?"
	}
	return "Wispr, what about ABC-42?"
}

// firstFrame is the first frame the vendor client sent the loopback socket.
func firstFrame(t *testing.T, frames <-chan []byte) []byte {
	t.Helper()
	select {
	case raw := <-frames:
		return raw
	case <-time.After(r7Deadline):
		t.Fatal("control: the vendor client sent no frame")
		return nil
	}
}

// TestVoiceCsOpeningPromptLeavesOutWhatWasTakenBack (Sol's fourteenth review
// of #191, P2): voice C opens before any answer, and its Settings frame
// gives both think models the room's recent words and the asker's words. At
// 2683f57 the history was cut only once a turn was handed to an answer, so
// "Wispr, what about ABC-42? never mind. what about ABC-12?", one final
// with no endpoint, reached the models whole, in both. Every copy of an
// exchange the Supervisor hands out now has the turn's take-back applied.
// Control: with no take-back, ABC-42 reaches the models.
func TestVoiceCsOpeningPromptLeavesOutWhatWasTakenBack(t *testing.T) {
	for _, taken := range []bool{true, false} {
		name := "control: no take-back"
		if taken {
			name = "a take-back in the opening final"
		}
		t.Run(name, func(t *testing.T) {
			frames := make(chan []byte, 1)
			upgrader := dgws.Upgrader{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				ws, err := upgrader.Upgrade(w, req, nil)
				if err != nil {
					return
				}
				defer func() { _ = ws.Close() }()
				if ws.WriteJSON(map[string]string{"type": "Welcome"}) != nil {
					return
				}
				_, raw, err := ws.ReadMessage()
				if err != nil {
					return
				}
				frames <- raw
				_ = ws.WriteJSON(map[string]string{"type": "SettingsApplied"})
				for {
					if _, _, err := ws.ReadMessage(); err != nil {
						return
					}
				}
			}))
			t.Cleanup(server.Close)
			agent, err := dgagent.New(&dgagent.Config{APIKey: "loopback-not-a-secret", URL: "ws" + strings.TrimPrefix(server.URL, "http")})
			if err != nil {
				t.Fatal(err)
			}
			r := openingRig(t, agent)
			r.hearNow(openingWords(taken), false)
			var settings struct {
				Agent struct {
					Think []struct {
						Prompt string `json:"prompt"`
					} `json:"think"`
				} `json:"agent"`
			}
			if err := json.Unmarshal(firstFrame(t, frames), &settings); err != nil {
				t.Fatal(err)
			}
			if len(settings.Agent.Think) == 0 {
				t.Fatal("control: the Settings frame names no think model")
			}
			for i, think := range settings.Agent.Think {
				_, history, found := strings.Cut(think.Prompt, "What the room said in the last two minutes, for context (it was not said to you):\n")
				if !found {
					t.Fatalf("control: think model %d's prompt holds no recent words", i)
				}
				history, asker, found := strings.Cut(history, "The room's transcript heard the person calling you say: ")
				if !found {
					t.Fatalf("control: think model %d's prompt holds no asker's words", i)
				}
				for part, text := range map[string]string{"recent words": history, "asker's words": asker} {
					if got := strings.Contains(text, "ABC-42"); got == taken {
						t.Errorf("think model %d's %s hold ABC-42: %v, want %v:\n%s", i, part, got, !taken, text)
					}
				}
			}
			r.model.mu.Lock()
			n := len(r.model.requests)
			r.model.mu.Unlock()
			if n != 0 {
				t.Fatalf("control: the Concierge called its model %d times; the frame was not before the answer", n)
			}
		})
	}
}

// TestVoiceBsOpeningLeavesOutWhatWasTakenBack (Sol's fourteenth review of
// #191, P2): voice B opens before any answer too, and its session.start
// gives GPT-Live the room's recent words and the asker's words. At 2683f57
// "Wispr, what about ABC-42? never mind. what about ABC-12?" reached it
// whole. Control: with no take-back, ABC-42 reaches it.
func TestVoiceBsOpeningLeavesOutWhatWasTakenBack(t *testing.T) {
	for _, taken := range []bool{true, false} {
		name := "control: no take-back"
		if taken {
			name = "a take-back in the opening final"
		}
		t.Run(name, func(t *testing.T) {
			frames := make(chan []byte, 1)
			upgrader := websocket.Upgrader{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				ws, err := upgrader.Upgrade(w, req, nil)
				if err != nil {
					return
				}
				defer func() { _ = ws.Close() }()
				_, raw, err := ws.ReadMessage()
				if err != nil {
					return
				}
				frames <- raw
				for {
					if _, _, err := ws.ReadMessage(); err != nil {
						return
					}
				}
			}))
			t.Cleanup(server.Close)
			live, err := gptlive.New(gptlive.Config{
				URL:   "ws" + strings.TrimPrefix(server.URL, "http"),
				Token: func(context.Context) (string, error) { return "loopback-not-a-secret", nil },
			})
			if err != nil {
				t.Fatal(err)
			}
			r := openingRig(t, live)
			r.hearNow(openingWords(taken), false)
			var start struct {
				Type    string `json:"type"`
				Session struct {
					Input json.RawMessage `json:"input"`
				} `json:"session"`
			}
			if err := json.Unmarshal(firstFrame(t, frames), &start); err != nil {
				t.Fatal(err)
			}
			input := string(start.Session.Input)
			if !strings.Contains(input, "ABC-12") && taken {
				t.Fatalf("control: the session start's input holds no room words: %s", input)
			}
			if got := strings.Contains(input, "ABC-42"); got == taken {
				t.Errorf("the session start's input holds ABC-42: %v, want %v:\n%s", got, !taken, input)
			}
		})
	}
}

// TestAQuoteThatBeginsAFinalHoldsTheQuestion (Sol's fourteenth review of
// #191, a diagnostic): after Wispr's question, "does ABC-12 show" and a
// final that begins with its quotation, `”no, cancel, then retry” in the
// tooltip?`, are one question. At 2683f57, with an endpoint between them,
// the answer read the second final alone: the quotation began where that
// final began, and only one that began before it counted. Controls: the
// same with no endpoint.
func TestAQuoteThatBeginsAFinalHoldsTheQuestion(t *testing.T) {
	for _, mark := range []string{"”", `"`} {
		for _, endpoints := range []bool{false, true} {
			t.Run(endpointNames(mark, endpoints), func(t *testing.T) {
				finals := []string{"does ABC-12 show", mark + "no, cancel, then retry" + mark + " in the tooltip?"}
				r := askedBackRig(t)
				r.hearReply(finals, endpoints)
				if out := r.answer(t, "reply", finals[len(finals)-1]); out.Say == "" {
					t.Fatal("the reply was not answered")
				}
				if got, want := modelQuestion(r), strings.Join(finals, " "); got != want {
					t.Errorf("the model was asked %q, want %q", got, want)
				}
				if recent := recentWords(t, r); !strings.Contains(recent, finals[0]) {
					t.Errorf("the recent words lost %q:\n%s", finals[0], recent)
				}
			})
		}
	}
}
