package config

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
)

// TestAVertexAIProviderReadsItsLabels fails if a Vertex AI provider's labels
// do not load from the file, warn though Vertex AI takes them, or are not
// the labels its generate calls carry (GenerateLabels).
func TestAVertexAIProviderReadsItsLabels(t *testing.T) {
	clearVoiceEnv(t)
	cfg, warnings, err := Load(writeConfig(t, `
providers:
  vertex:
    api: vertex
    project: example-project
    auth: {kind: google}
    labels:
      owner: example-owner
      stream: example-stream
`))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"owner": "example-owner", "stream": "example-stream"}
	if got := cfg.Providers["vertex"].Labels; !maps.Equal(got, want) {
		t.Fatalf("providers.vertex.labels = %v, want %v", got, want)
	}
	if vertex := cfg.Providers["vertex"]; !maps.Equal(vertex.GenerateLabels(), want) {
		t.Fatalf("GenerateLabels = %v, want the provider's labels %v", vertex.GenerateLabels(), want)
	}
	if slices.ContainsFunc(warnings, func(w string) bool { return strings.Contains(w, "providers.vertex") }) {
		t.Fatalf("labels Vertex AI takes warned: %q", warnings)
	}
}

// TestGenerateLabelsAreVertexAIsAlone fails if a generate call on a provider
// of another API than Vertex AI would carry labels: only Vertex AI bills by
// them, and genai would send them on to the Gemini API, a model gateway's
// Google routes among its callers.
func TestGenerateLabelsAreVertexAIsAlone(t *testing.T) {
	labels := map[string]string{"owner": "example-owner"}
	for _, api := range APIs {
		if api == APIVertex {
			continue
		}
		if got := (&Provider{API: api, Labels: labels}).GenerateLabels(); got != nil {
			t.Errorf("GenerateLabels on the %s API = %v, want none", api, got)
		}
	}
}

// TestLabelsVertexAIWouldRefuseWarn fails if labels on a provider that is not
// Vertex AI, or labels that break Vertex AI's rules (a key of 1 to 63
// lowercase or international letters, digits, underscores or dashes that
// starts with a letter; a value of up to 63 of them; at most 64 labels a
// call), start without a warning that names the provider's labels, or if such
// labels are still the ones its generate calls carry: Vertex AI would refuse
// every call.
func TestLabelsVertexAIWouldRefuseWarn(t *testing.T) {
	vertex := func(labels string) string {
		return "{api: vertex, project: example-project, auth: {kind: google}, labels: " + labels + "}"
	}
	many := make([]string, 65)
	for i := range many {
		many[i] = fmt.Sprintf("k%d: v", i)
	}
	cases := map[string]string{
		"on the gemini api":       "{api: gemini, auth: {kind: api_key, env: EXAMPLE_KEY}, labels: {owner: example-owner}}",
		"on the openai api":       "{api: openai, auth: {kind: api_key, env: EXAMPLE_KEY}, labels: {owner: example-owner}}",
		"upper-case key":          vertex("{Owner: example-owner}"),
		"key starts with a digit": vertex("{1owner: example-owner}"),
		"key starts with a dash":  vertex("{-owner: example-owner}"),
		"empty key":               vertex("{'': example-owner}"),
		"key too long":            vertex("{" + strings.Repeat("k", 64) + ": v}"),
		"upper-case value":        vertex("{owner: Example}"),
		"value with a space":      vertex("{owner: 'an owner'}"),
		"value with a dot":        vertex("{owner: a.owner}"),
		"value too long":          vertex("{owner: " + strings.Repeat("v", 64) + "}"),
		"too many labels":         vertex("{" + strings.Join(many, ", ") + "}"),
	}
	for name, provider := range cases {
		t.Run(name, func(t *testing.T) {
			clearVoiceEnv(t)
			cfg, warnings, err := Load(writeConfig(t, "providers:\n  broken: "+provider+"\n"))
			if err != nil {
				t.Fatal(err)
			}
			if !slices.ContainsFunc(warnings, func(w string) bool { return strings.Contains(w, "providers.broken.labels") }) {
				t.Errorf("warnings %q name no providers.broken.labels", warnings)
			}
			if broken := cfg.Providers["broken"]; broken.GenerateLabels() != nil {
				t.Errorf("GenerateLabels = %v, want none: Vertex AI would refuse every call that carried them", broken.GenerateLabels())
			}
		})
	}
	t.Run("labels Vertex AI takes", func(t *testing.T) {
		clearVoiceEnv(t)
		most := make([]string, 64)
		for i := range most {
			most[i] = fmt.Sprintf("k%d: v", i)
		}
		for _, labels := range []string{
			"{owner: example-owner, stream: example_stream-2, empty: '', 名前: 値}",
			"{" + strings.Repeat("k", 63) + ": " + strings.Repeat("v", 63) + "}",
			"{" + strings.Join(most, ", ") + "}",
		} {
			cfg, warnings, err := Load(writeConfig(t, "providers:\n  whole: "+vertex(labels)+"\n"))
			if err != nil {
				t.Fatal(err)
			}
			if slices.ContainsFunc(warnings, func(w string) bool { return strings.Contains(w, "providers.") }) {
				t.Errorf("labels %s warned: %q", labels, warnings)
			}
			if whole := cfg.Providers["whole"]; len(whole.GenerateLabels()) == 0 {
				t.Errorf("labels %s: GenerateLabels gave none", labels)
			}
		}
	})
}
