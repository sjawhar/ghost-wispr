package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestASavedOldDefaultDeepgramBufferIsReadAsTheNewOne fails if
// deepgram_buffer_size: 1920000, the old default that a settings save under an
// earlier build wrote, is kept in bytes: 60 s at 16000 Hz, but 20 s at 48000.
// It must read as 0, 60 s at the microphone's rate. It also fails if a save
// drops a size a person set while it writes another setting to local settings.
func TestASavedOldDefaultDeepgramBufferIsReadAsTheNewOne(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvPrefix+"DEEPGRAM_BUFFER_SIZE", "")
	for _, tc := range []struct {
		name string
		file string
		want int
	}{
		{name: "unset", file: "", want: 0},
		{name: "the old default a save wrote", file: "deepgram_buffer_size: 1920000\n", want: 0},
		{name: "a size a person set", file: "deepgram_buffer_size: 1000000\n", want: 1000000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "ghost-wispr.yaml")
			if err := os.WriteFile(path, []byte("speaker_enabled: false\n"+tc.file), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, _, err := Load(path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.DeepgramBufferSize != tc.want {
				t.Errorf("deepgram_buffer_size reads %d, want %d", cfg.DeepgramBufferSize, tc.want)
			}

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
			if got := restarted.Get().DeepgramBufferSize; got != tc.want {
				t.Errorf("deepgram_buffer_size after a settings save = %d, want %d", got, tc.want)
			}
		})
	}
}
