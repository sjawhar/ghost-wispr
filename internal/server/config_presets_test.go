package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/config"
)

// presetAfter sends body as PATCH /api/config and returns the standup preset
// it answers with.
func presetAfter(t *testing.T, h http.Handler, body string) config.Preset {
	t.Helper()
	rr := patchConfig(h, body)
	if rr.Code != http.StatusOK {
		t.Fatalf("PATCH %s = %d %s, want 200", body, rr.Code, rr.Body.String())
	}
	var resp struct {
		Summarization struct {
			Presets map[string]config.Preset `json:"presets"`
		} `json:"summarization"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp.Summarization.Presets["standup"]
}

// TestPatchConfigClearsAPresetsModelAndDescription fails if a PATCH that
// names a preset's model or description as "" keeps the old value, as it did:
// the settings page could not take a preset's model override off, so the
// preset kept its own model instead of summarization.model. It also fails if a
// field the patch leaves out changes, or if "" empties the system prompt or
// the user template, which a preset needs.
func TestPatchConfigClearsAPresetsModelAndDescription(t *testing.T) {
	cfgStore, path := newProvidersConfigStore(t)
	h := newConfigHandler(t, cfgStore, &ControlHooks{})
	full := config.Preset{
		Description:  "Stand-up",
		SystemPrompt: "Summarize briefly",
		UserTemplate: "{{transcript}}",
		Model:        "vertex/gemini-3.5-flash-lite",
	}
	if got := presetAfter(t, h, `{"summarization":{"presets":{"standup":{"description":"Stand-up","system_prompt":"Summarize briefly","user_template":"{{transcript}}","model":"vertex/gemini-3.5-flash-lite"}}}}`); got != full {
		t.Fatalf("created preset = %+v, want %+v", got, full)
	}

	// The control: a patch that leaves the model out keeps it.
	kept := full
	kept.Description = "Daily stand-up"
	if got := presetAfter(t, h, `{"summarization":{"presets":{"standup":{"description":"Daily stand-up"}}}}`); got != kept {
		t.Fatalf("after a description patch the preset = %+v, want %+v", got, kept)
	}

	cleared := kept
	cleared.Model = ""
	cleared.Description = ""
	if got := presetAfter(t, h, `{"summarization":{"presets":{"standup":{"model":"","description":""}}}}`); got != cleared {
		t.Errorf("after a patch of model and description \"\" the preset = %+v, want %+v", got, cleared)
	}

	if got := presetAfter(t, h, `{"summarization":{"presets":{"standup":{"system_prompt":"","user_template":""}}}}`); got != cleared {
		t.Errorf("after a patch of system_prompt and user_template \"\" the preset = %+v, want %+v (kept)", got, cleared)
	}

	reloaded, _, err := config.NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.Get().Summarization.Presets["standup"]; got != cleared {
		t.Errorf("standup saved in the config file = %+v, want %+v", got, cleared)
	}
}
