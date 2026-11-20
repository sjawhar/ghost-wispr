package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestReadmeStatesVoiceConsent fails if the README's voice section, or the
// office Pi's posted recording rules, leave out what the meeting voice hears,
// where the room's words go, or what a deletion cannot recall: a question sent
// to an agent leaves the unit (plan "Decisions": deletion; dispatch://INFRA-4592).
func TestReadmeStatesVoiceConsent(t *testing.T) {
	readme := readmeText(t)
	for _, words := range []string{
		"The voice hears the room only while the light reads red",
		"While capture is paused, nothing hears the room: no wake phrase is heard",
		"Voice A, text-to-speech (`voice.engine: tts`), sends no room audio anywhere",
		"The asker's words after the wake phrase go to the Concierge's model",
		"The text Wispr speaks goes to the text-to-speech provider",
		"A question sent to an agent leaves the unit, and deleting the segment or the session cannot recall it",
		"Deleting a session erases its questions and the answers the agents wrote",
		"An agent's written answer may quote the asker, and only a session delete erases it: a segment delete does not",
		"The unit stores none of the room's words for a question it sent",
		"Every pause, light hold, press of the Jabra's Mute button, \"Wispr, stop\" and shutdown stop the voice at once",
		"With `voice.engine: dgagent` (voice C, a voice agent), during an exchange only",
		"hands the turn, with the same transcript, to OpenAI's gpt-4.1-mini",
		"a held answer also opens voice C without one",
		"Between exchanges nothing goes to the Voice Agent",
		"Voice B, speech-to-speech (`voice.engine: gptlive`), sends room audio to OpenAI's GPT-Live",
		"from the wake phrase to the end of the exchange: nothing said before the wake phrase goes",
		"a held answer also opens voice B without one",
		"Between exchanges nothing goes to OpenAI",
		"up to about 1.4 s of them never played",
		"matching calls and commands can be ignored for up to 30 s while Wispr speaks or its echo can still be heard",
		// R7 and R10: when an answer may speak, and the sounds.
		"It speaks by itself only when nobody has spoken since the question and the light still reads red",
		"Every gate closing, a pause or a light hold, ends the recording session, and an ended session's answers are never spoken: they stay in writing on that session's page",
		"Wispr plays a rising three-note sound twice after a second in which nobody speaks",
		"For 6 s after Wispr's answer, a follow-up that the transcript ends with a question mark needs no wake phrase",
		"Anything else said in those 6 s is side talk: no voice answers it, and nothing of it goes to Dispatch",
		"the live Deepgram request carries them, and Dispatch's project keys, as Nova-3 keyterms",
		"\"In for\" reads as INFRA only before a number of three or more digits",
		"\"Wispr, never mind\" cancels the newest question to an agent",
		"a single high note when capture starts recording",
		"a falling two-note sound when Wispr cannot do what was asked",
		// R13: the settings page.
		"The settings page turns the voice on or off and picks the voice and the text-to-speech provider, with no restart",
		"With the voice off, the unit reads no wake phrase and runs no Concierge",
		// R20: every text recipient, and vendor errors.
		"so the question also reaches that model's vendor",
		"A speech vendor's error message that quotes the text to speak is left out of the journal",
		"`voice_tasks_changed`",
	} {
		if !strings.Contains(readme, words) {
			t.Errorf("the README does not say %q", words)
		}
	}

	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "pi", "files", "ghost-wispr.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var pi struct {
		RecordingRules []string `yaml:"recording_rules"`
		Voice          struct {
			Enabled        bool `yaml:"enabled"`
			PausedListener bool `yaml:"paused_listener"`
		} `yaml:"voice"`
	}
	if err := yaml.Unmarshal(raw, &pi); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, rule := range pi.RecordingRules {
		if strings.Contains(rule, "Wispr") && strings.Contains(rule, "leaves the unit") {
			found = true
		}
	}
	if pi.Voice.Enabled && !found {
		t.Errorf("the Pi turns the voice on, but no recording rule says that a question to an agent leaves the unit: %q", pi.RecordingRules)
	}
	if pi.Voice.PausedListener {
		t.Error("the Pi turns the paused listener on before its false-wake limit is measured")
	}
}
