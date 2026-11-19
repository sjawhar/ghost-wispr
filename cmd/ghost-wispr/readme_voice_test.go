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
		"Voice A sends no room audio anywhere",
		"The asker's words after the wake phrase go to the Concierge's model",
		"The text Wispr speaks goes to the text-to-speech engine",
		"A question sent to an agent leaves the unit, and deleting the segment or the session cannot recall it",
		"Deleting a session erases its questions and the answers the agents wrote",
		"An agent's written answer may quote the asker, and only a session delete erases it: a segment delete does not",
		"The unit keeps no words of the room for a question it sent",
		"Every pause, the Jabra's Mute button and \"Wispr, stop\" stop the voice at once",
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
