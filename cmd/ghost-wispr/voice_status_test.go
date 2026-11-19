package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// TestAVoiceThatCannotBeBuiltWarnsOnTheStatusPage fails if a voice the
// settings name but cannot build (a missing id or key) shows only at the
// first call, as the error sound, instead of on /api/status from the moment
// the settings are applied; or if the warning stays once the voice builds, or
// while the voice is off.
func TestAVoiceThatCannotBeBuiltWarnsOnTheStatusPage(t *testing.T) {
	warnings := newWarningsRegistry()
	var voices voiceSource
	voices.set("k", nil, errors.New("voice.gptlive.openai_identity_provider_id is missing"))
	voices.warnBuild(true, warnings)
	if got := strings.Join(warnings.List(), "\n"); !strings.Contains(got, "openai_identity_provider_id is missing") {
		t.Fatalf("status warnings = %q, want the voice's build error", got)
	}
	voices.warnBuild(false, warnings)
	if got := warnings.List(); len(got) != 0 {
		t.Fatalf("with the voice off, status warnings = %q, want none", got)
	}
	voices.warnBuild(true, warnings)
	voices.set("k", builtVoice{}, nil)
	voices.warnBuild(true, warnings)
	if got := warnings.List(); len(got) != 0 {
		t.Fatalf("once the voice builds, status warnings = %q, want none", got)
	}
}

// builtVoice is a voice that built; no call opens it here.
type builtVoice struct{}

func (builtVoice) Open(context.Context, *voice.Exchange) (voice.Session, error) {
	return nil, errors.New("not opened in this test")
}
