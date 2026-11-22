package server

import (
	"net/http"
	"testing"
)

// settingsPageBodies are the bodies the settings page sends to PATCH
// /api/config, one for each patchConfig call in web/src/components, built as
// each one builds its patch. PATCH /api/config refuses a field it does not
// know, so a body here that names one would make the page report a failure
// for a save it used to take.
var settingsPageBodies = map[string]string{
	"GeneralSettings: every field":     `{"silence_timeout":"45s","summarization":{"model":"vertex/gemini-example"},"transcription":{"endpointing":"300","utterance_end_ms":"1000"}}`,
	"IntegrationsSettings: folder":     `{"gdrive":{"folder_id":"folder-example"}}`,
	"IntegrationsSettings: sync":       `{"gdrive":{"sync_enabled":true}}`,
	"IntegrationsSettings: gc":         `{"gc":{"enabled":true,"max_age_days":0,"max_audio_size_mb":1024}}`,
	"ModelProviders: a key":            `{"provider_keys":{"deepgram":"dg-2"}}`,
	"PresetEditor: save":               `{"summarization":{"presets":{"standup":{"description":"Stand-up","system_prompt":"Summarize briefly","user_template":"{{transcript}}","model":""}}}}`,
	"PresetEditor: clear the model":    `{"summarization":{"presets":{"standup":{"model":""}}}}`,
	"PresetEditor: delete":             `{"summarization":{"presets":{"standup":null}}}`,
	"VoiceSettings: voice and its tts": `{"voice":{"enabled":true,"engine":"tts"},"tts":{"model":"vertex-us/gemini-3.8-flash-tts","voice":"Puck"}}`,
	"IntegrationsSettings: gc limit":   `{"gc":{"max_age_days":0}}`,
	"PresetEditor: change a field":     `{"summarization":{"presets":{"standup":{"description":"Daily stand-up"}}}}`,
}

// heldPreset creates the standup preset the page's preset bodies change.
const heldPreset = `{"summarization":{"presets":{"standup":{"description":"Stand-up","system_prompt":"Summarize briefly","user_template":"{{transcript}}","model":""}}}}`

// TestPatchConfigTakesEveryBodyTheSettingsPageSends fails if PATCH
// /api/config refuses a body the settings page sends, or takes a field it
// does not know (the control: api_keys, which the page no longer sends).
func TestPatchConfigTakesEveryBodyTheSettingsPageSends(t *testing.T) {
	for name, body := range settingsPageBodies {
		t.Run(name, func(t *testing.T) {
			cfgStore, _ := newProvidersConfigStore(t)
			h := newConfigHandler(t, cfgStore, &ControlHooks{})
			// The page saves a preset's changed fields only to a preset it
			// shows, one the unit holds.
			if rr := patchConfig(h, heldPreset); rr.Code != http.StatusOK {
				t.Fatalf("PATCH %s = %d %s, want 200", heldPreset, rr.Code, rr.Body.String())
			}
			if rr := patchConfig(h, body); rr.Code != http.StatusOK {
				t.Fatalf("PATCH %s = %d %s, want 200", body, rr.Code, rr.Body.String())
			}
		})
	}
	t.Run("control: an unknown field", func(t *testing.T) {
		cfgStore, _ := newProvidersConfigStore(t)
		h := newConfigHandler(t, cfgStore, &ControlHooks{})
		if rr := patchConfig(h, `{"api_keys":{"deepgram":"dg-2"}}`); rr.Code != http.StatusBadRequest {
			t.Fatalf("PATCH api_keys = %d %s, want 400", rr.Code, rr.Body.String())
		}
	})
}

// TestPatchConfigRefusesInputAfterItsObject fails if PATCH /api/config takes
// a body with anything after its JSON object, saving the first object and
// ignoring the rest, as it did before: an unknown field after it is no more
// acceptable than one inside it.
func TestPatchConfigRefusesInputAfterItsObject(t *testing.T) {
	for name, body := range map[string]string{
		"a second object":    `{"silence_timeout":"19s"} {"unknown_setting":true}`,
		"a stray value":      `{"silence_timeout":"19s"} 1`,
		"a second good body": `{"silence_timeout":"19s"} {"silence_timeout":"21s"}`,
	} {
		t.Run(name, func(t *testing.T) {
			cfgStore, _ := newProvidersConfigStore(t)
			before := cfgStore.Get().SilenceTimeout
			h := newConfigHandler(t, cfgStore, &ControlHooks{})
			if rr := patchConfig(h, body); rr.Code != http.StatusBadRequest {
				t.Fatalf("PATCH %s = %d %s, want 400", body, rr.Code, rr.Body.String())
			}
			if got := cfgStore.Get().SilenceTimeout; got != before {
				t.Fatalf("a refused PATCH saved silence_timeout %q (was %q)", got, before)
			}
		})
	}
	t.Run("control: one object, whitespace after", func(t *testing.T) {
		cfgStore, _ := newProvidersConfigStore(t)
		h := newConfigHandler(t, cfgStore, &ControlHooks{})
		if rr := patchConfig(h, "{\"silence_timeout\":\"19s\"}\n  "); rr.Code != http.StatusOK || cfgStore.Get().SilenceTimeout != "19s" {
			t.Fatalf("PATCH of one object = %d %s, silence_timeout %q; want 200 and 19s", rr.Code, rr.Body.String(), cfgStore.Get().SilenceTimeout)
		}
	})
}
