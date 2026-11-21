package config

import "testing"

// TestAnExplicitEmptyProvidersSectionStaysEmpty catches adding quickstart
// providers to a file that explicitly declares none, including after a save.
func TestAnExplicitEmptyProvidersSectionStaysEmpty(t *testing.T) {
	clearVoiceEnv(t)
	for _, section := range []string{"providers: {}\n", "providers: null\n", "providers:\n"} {
		t.Run(section, func(t *testing.T) {
			path := writeConfig(t, section)
			store, _, err := NewStore(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(store.Get().Providers); got != 0 {
				t.Fatalf("explicit empty providers loaded %d quickstart providers", got)
			}
			if err := store.Update(func(c *Config) { c.SilenceTimeout = "45s" }); err != nil {
				t.Fatal(err)
			}
			reloaded, _, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if len(reloaded.Providers) != 0 || reloaded.Transcription.Model != "" || reloaded.Summarization.Model != "" {
				t.Fatalf("save restored quickstart providers or roles: %+v", reloaded)
			}
		})
	}
}
