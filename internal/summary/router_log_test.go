package summary

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/llm"
)

// TestARouterReplyThatIsNoPresetIsNotLogged: the preset router sends the
// model up to 700 words of the transcript and asks for a preset's name. A
// reply that is not one can quote the conversation; the app's log says the
// reply matched no preset, and never what it said.
func TestARouterReplyThatIsNoPresetIsNotLogged(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	cfg := config.Summarization{
		Model:   "openai/gpt-4o-mini",
		Presets: map[string]config.Preset{"default": {Description: "General"}, "meeting": {Description: "Meetings"}},
	}
	router := NewRouter(cfg, func(_, _ string) (llm.Client, error) {
		return &mockLLMClient{response: "This talk about the zyxquorbelfin budget fits meeting"}, nil
	})
	chosen, err := router.SelectPreset(context.Background(), "We set the zyxquorbelfin budget.")
	if err != nil || chosen != "default" {
		t.Fatalf("SelectPreset = %q, %v; want the default preset", chosen, err)
	}
	if !strings.Contains(logs.String(), "falling back to default preset") {
		t.Fatalf("control: the log does not say the router fell back: %s", logs.String())
	}
	if strings.Contains(logs.String(), "zyxquorbelfin") {
		t.Errorf("the log holds the model's reply: %s", logs.String())
	}
}
