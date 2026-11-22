package main

import (
	"maps"
	"strings"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/config"
)

// TestAVoiceThatCannotBeBuiltWarnsOnTheStatusPage fails if a voice the
// settings name but cannot build (here voice B on an OpenAI provider that
// signs in by workload identity and names neither of its ids) shows only at
// the first call, as the error sound, instead of on /api/status from the
// moment the settings are applied; or if the warning stays once the voice
// builds, or while the voice is off.
func TestAVoiceThatCannotBeBuiltWarnsOnTheStatusPage(t *testing.T) {
	warnings := newWarningsRegistry()
	cfg := deepgramKeyConfig("dg-1")
	cfg.Providers["openai"] = config.Provider{API: config.APIOpenAI, Auth: config.ProviderAuth{Kind: config.AuthOpenAIWIF}}
	cfg.Voice.Engine, cfg.Voice.GPTLive.Model = config.VoiceEngineGPTLive, "openai/gpt-live-1"
	var voices voiceSource
	voices.apply(t.Context(), &cfg)
	voices.warnBuild(true, warnings)
	if got := strings.Join(warnings.List(), "\n"); !strings.Contains(got, "set auth.identity_provider_id and auth.service_account_id") {
		t.Fatalf("status warnings = %q, want the voice's build error naming the provider's missing ids", got)
	}
	voices.warnBuild(false, warnings)
	if got := warnings.List(); len(got) != 0 {
		t.Fatalf("with the voice off, status warnings = %q, want none", got)
	}
	voices.warnBuild(true, warnings)
	cfg.Providers = maps.Clone(cfg.Providers)
	cfg.Providers["openai"] = config.Provider{API: config.APIOpenAI, Auth: config.ProviderAuth{Kind: config.AuthOpenAIWIF, IdentityProviderID: "idp_example", ServiceAccountID: "user-example"}}
	voices.apply(t.Context(), &cfg)
	voices.warnBuild(true, warnings)
	if got := warnings.List(); len(got) != 0 {
		t.Fatalf("once the voice builds, status warnings = %q, want none", got)
	}
}
