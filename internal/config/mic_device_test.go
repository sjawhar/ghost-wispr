package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMicDeviceComesFromTheConfigAndTheEnvironment fails if mic_device, the
// sound card the microphone opens, is not read from ghost-wispr.yaml, is not
// overridden by GHOST_WISPR_MIC_DEVICE (as an injected-audio acceptance run
// sets it to Loopback for its run), or is lost when a settings save writes
// another setting to local settings. Unset, it is empty: the microphone opens
// the default input, as before the setting existed.
func TestMicDeviceComesFromTheConfigAndTheEnvironment(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvPrefix+"MIC_DEVICE", "")

	cfg, _, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MicDevice != "" {
		t.Fatalf("mic_device unset = %q, want empty: the default input", cfg.MicDevice)
	}

	path := filepath.Join(t.TempDir(), "ghost-wispr.yaml")
	if err := os.WriteFile(path, []byte("mic_device: Jabra\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _, err = Load(path)
	if err != nil {
		t.Fatalf("Load %s: %v", path, err)
	}
	if cfg.MicDevice != "Jabra" {
		t.Fatalf("mic_device from the config = %q, want Jabra", cfg.MicDevice)
	}

	t.Setenv(EnvPrefix+"MIC_DEVICE", "Loopback")
	cfg, _, err = Load(path)
	if err != nil {
		t.Fatalf("Load %s with GHOST_WISPR_MIC_DEVICE: %v", path, err)
	}
	if cfg.MicDevice != "Loopback" {
		t.Fatalf("mic_device with GHOST_WISPR_MIC_DEVICE=Loopback = %q, want Loopback", cfg.MicDevice)
	}

	t.Setenv(EnvPrefix+"MIC_DEVICE", "")
	store, _, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.Update(func(c *Config) { c.SpeakerEnabled = true }); err != nil {
		t.Fatalf("Update: %v", err)
	}
	restarted, _, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore after a settings save: %v", err)
	}
	if got := restarted.Get().MicDevice; got != "Jabra" {
		t.Fatalf("a settings save dropped mic_device: %q", got)
	}
}
