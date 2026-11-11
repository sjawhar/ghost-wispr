package main

import (
	"bytes"
	"context"
	"testing"
)

func TestGoogleIDTokenSubcommand(t *testing.T) {
	var output bytes.Buffer
	var gotAudience string
	handled, err := runGoogleIDTokenCommand(
		context.Background(),
		[]string{"google-id-token", "--audience", "https://tailscale.example.test"},
		&output,
		"/opt/ghost-wispr/google-credentials.json",
		func(_ context.Context, credentialPath, audience string) (string, error) {
			if credentialPath != "/opt/ghost-wispr/google-credentials.json" {
				t.Fatalf("credential path = %q", credentialPath)
			}
			gotAudience = audience
			return "signed-id-token", nil
		},
	)
	if err != nil {
		t.Fatalf("runGoogleIDTokenCommand: %v", err)
	}
	if !handled {
		t.Fatal("google-id-token command was not handled")
	}
	if gotAudience != "https://tailscale.example.test" {
		t.Fatalf("audience = %q", gotAudience)
	}
	if output.String() != "signed-id-token\n" {
		t.Fatalf("stdout = %q, want token alone", output.String())
	}
}

func TestGoogleIDTokenSubcommandCredentialsOverride(t *testing.T) {
	var output bytes.Buffer
	_, err := runGoogleIDTokenCommand(
		context.Background(),
		[]string{"google-id-token", "--audience", "https://tailscale.example.test", "--credentials", "/override.json"},
		&output,
		"/opt/ghost-wispr/google-credentials.json",
		func(_ context.Context, credentialPath, _ string) (string, error) {
			if credentialPath != "/override.json" {
				t.Fatalf("credential path = %q, want override", credentialPath)
			}
			return "signed-id-token", nil
		},
	)
	if err != nil {
		t.Fatalf("runGoogleIDTokenCommand: %v", err)
	}
}

func TestGoogleIDTokenSubcommandAllowsADCFallback(t *testing.T) {
	var output bytes.Buffer
	handled, err := runGoogleIDTokenCommand(
		context.Background(),
		[]string{"google-id-token", "--audience", "https://tailscale.example.test"},
		&output,
		"",
		func(_ context.Context, credentialPath, _ string) (string, error) {
			if credentialPath != "" {
				t.Fatalf("credential path = %q, want ADC fallback", credentialPath)
			}
			return "signed-id-token", nil
		},
	)
	if err != nil || !handled {
		t.Fatalf("handled=%t err=%v", handled, err)
	}
}
