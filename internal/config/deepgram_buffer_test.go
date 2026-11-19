package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestASavedOldDefaultDeepgramBufferIsReadAsTheNewOne fails if
// deepgram_buffer_size: 1920000, the old default that a settings save under an
// earlier build wrote into ghost-wispr.yaml (Store.writeYAML writes the whole
// config), is kept in bytes: 60 s at 16000 Hz, but 20 s at 48000. It must
// read as 0, 60 s at the microphone's rate (ReviewPR78 on #78). It also fails
// if a save writes a default buffer size back to the file, or drops a size a
// person set.
func TestASavedOldDefaultDeepgramBufferIsReadAsTheNewOne(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvPrefix+"DEEPGRAM_BUFFER_SIZE", "")
	for _, tc := range []struct {
		name     string
		file     string
		want     int
		wantLine string // the deepgram_buffer_size line a save leaves; "" for none
	}{
		{name: "unset", file: "", want: 0},
		{name: "the old default a save wrote", file: "deepgram_buffer_size: 1920000\n", want: 0},
		{name: "a size a person set", file: "deepgram_buffer_size: 1000000\n", want: 1000000, wantLine: "deepgram_buffer_size: 1000000"},
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
			// A settings save of another key writes the whole config back.
			if err := store.Update(func(c *Config) { c.SpeakerEnabled = true }); err != nil {
				t.Fatalf("Update: %v", err)
			}
			saved, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var line string
			for _, l := range strings.Split(string(saved), "\n") {
				if strings.HasPrefix(l, "deepgram_buffer_size:") {
					line = l
				}
			}
			if line != tc.wantLine {
				t.Errorf("after a settings save the file holds %q, want %q", line, tc.wantLine)
			}
		})
	}
}
