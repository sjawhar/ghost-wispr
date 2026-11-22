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
// names a preset the config does not hold creates it unless it names all four
// of the preset's fields, with a user template (it carries the transcript), as
// the page's new preset does. It did: the settings page, which sends only a
// preset's changed fields, saved a description (or a template) to a preset
// another tab had deleted, and the unit recreated the preset with the other
// fields empty, an empty template leaving the transcript out of its
// summaries. It also fails if such a patch answers other than 404, or if a
// patch with any field cannot update a preset the config holds. (The built-in
// default preset has no system prompt; the summarizer has its own.)
func TestPatchConfigRefusesAPatchToAPresetItDoesNotHold(t *testing.T) {
	cfgStore, path := newProvidersConfigStore(t)
	h := newConfigHandler(t, cfgStore, &ControlHooks{})

	create := `{"summarization":{"presets":{"standup":{"description":"Stand-up","system_prompt":"Summarize briefly","user_template":"Summarize: {{transcript}}","model":""}}}}`
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
		// The same tab, with the template changed: the page sends only the
		// changed fields, so a template alone is an update, not a new preset.
		`{"summarization":{"presets":{"standup":{"user_template":"P2 template: {{transcript}}"}}}}`,
		`{"summarization":{"presets":{"standup":{"description":"Stand-up","system_prompt":"Summarize briefly","user_template":"{{transcript}}"}}}}`,
		`{"summarization":{"presets":{"standup":{"system_prompt":"Summarize briefly","model":""}}}}`,
		`{"summarization":{"presets":{"standup":{"description":"","system_prompt":"","user_template":"","model":""}}}}`,
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

	// The control: a patch that names all four fields, as the page's new
	// preset does, creates the preset again.
	if rr := patchConfig(h, create); rr.Code != http.StatusOK {
		t.Fatalf("PATCH %s after the delete = %d %s, want 200", create, rr.Code, rr.Body.String())
	}
	if preset, ok := storedPreset(cfgStore, "standup"); !ok || preset.UserTemplate != "Summarize: {{transcript}}" || preset.Description != "Stand-up" {
		t.Errorf("standup after a create = %+v (held %v), want the fields sent", preset, ok)
	}
}
