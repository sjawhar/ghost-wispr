package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/config"
)

// storedPreset returns the preset name in the config the store holds, and
// whether it holds one.
func storedPreset(cfgStore *config.Store, name string) (config.Preset, bool) {
	preset, ok := cfgStore.Get().Summarization.Presets[name]
	return preset, ok
}

// TestPatchConfigRefusesAPatchToAPresetItDoesNotHold fails if a PATCH that
// names a preset the config does not hold, without the user template a preset
// needs (it carries the transcript), creates it, as it did: the settings page,
// which sends only a preset's changed fields, saved a description to a preset
// another tab had deleted, and the unit recreated the preset with an empty
// user template, so its summaries left out the transcript. It also fails if
// such a patch answers other than 404, or if a patch with a user template
// cannot create a preset or one with any field cannot update a preset the
// config holds. (The built-in default preset has no system prompt; the
// summarizer has its own.)
func TestPatchConfigRefusesAPatchToAPresetItDoesNotHold(t *testing.T) {
	cfgStore, path := newProvidersConfigStore(t)
	h := newConfigHandler(t, cfgStore, &ControlHooks{})

	create := `{"summarization":{"presets":{"standup":{"description":"Stand-up","system_prompt":"Summarize briefly","user_template":"Summarize: {{transcript}}"}}}}`
	if rr := patchConfig(h, create); rr.Code != http.StatusOK {
		t.Fatalf("PATCH %s = %d %s, want 200", create, rr.Code, rr.Body.String())
	}
	update := `{"summarization":{"presets":{"standup":{"description":"Daily stand-up"}}}}`
	if rr := patchConfig(h, update); rr.Code != http.StatusOK {
		t.Fatalf("PATCH %s to a preset the config holds = %d %s, want 200", update, rr.Code, rr.Body.String())
	}
	if rr := patchConfig(h, `{"summarization":{"presets":{"standup":null}}}`); rr.Code != http.StatusOK {
		t.Fatalf("deleting standup = %d %s, want 200", rr.Code, rr.Body.String())
	}

	for _, body := range []string{
		update, // a tab that opened the preset before another deleted it
		`{"summarization":{"presets":{"standup":{"system_prompt":"Summarize briefly","model":""}}}}`,
		`{"summarization":{"presets":{"standup":{"user_template":""}}}}`,
	} {
		rr := patchConfig(h, body)
		var answer struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(rr.Body.Bytes(), &answer)
		if rr.Code != http.StatusNotFound || answer.Error == "" {
			t.Errorf("PATCH %s to a deleted preset = %d %s, want 404 with an error", body, rr.Code, rr.Body.String())
		}
		if preset, ok := storedPreset(cfgStore, "standup"); ok {
			t.Errorf("PATCH %s recreated standup as %+v", body, preset)
		}
	}

	reloaded, _, err := config.NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if preset, ok := reloaded.Get().Summarization.Presets["standup"]; ok {
		t.Errorf("the config file holds the deleted standup as %+v", preset)
	}

	// The control: a patch with a user template creates the preset again.
	if rr := patchConfig(h, `{"summarization":{"presets":{"standup":{"user_template":"Summarize: {{transcript}}"}}}}`); rr.Code != http.StatusOK {
		t.Fatalf("PATCH of a user template after the delete = %d %s, want 200", rr.Code, rr.Body.String())
	}
	if preset, ok := storedPreset(cfgStore, "standup"); !ok || preset.UserTemplate != "Summarize: {{transcript}}" {
		t.Errorf("standup after a create = %+v (held %v), want its user template", preset, ok)
	}
	if rr := patchConfig(h, create); rr.Code != http.StatusOK {
		t.Errorf("PATCH %s to the preset again = %d %s, want 200", create, rr.Code, rr.Body.String())
	}
}
