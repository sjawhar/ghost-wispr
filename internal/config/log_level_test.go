package config

import (
	"strings"
	"testing"
)

// A log level that names no level gives a warning naming the value, where it
// was set (log_level in the file, or GHOST_WISPR_LOG_LEVEL) and the level the
// app logs at instead; a known level, in any case, gives none.
func TestAnUnknownLogLevelWarns(t *testing.T) {
	for _, tc := range []struct {
		name, yaml, env, where string
	}{
		{"misspelled in the file", "log_level: degub\n", "", "log_level"},
		{"misspelled in the environment", "log_level: info\n", "degub", EnvPrefix + "LOG_LEVEL"},
		{"known, in upper case", "log_level: WARN\n", "", ""},
		{"unset", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			if tc.env != "" {
				t.Setenv(EnvPrefix+"LOG_LEVEL", tc.env)
			}
			_, warnings, err := Load(writeConfig(t, tc.yaml))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			var about []string
			for _, w := range warnings {
				if strings.Contains(strings.ToLower(w), "log_level") {
					about = append(about, w)
				}
			}
			if tc.where == "" {
				if len(about) != 0 {
					t.Errorf("a known log level warned: %q", about)
				}
				return
			}
			if len(about) != 1 || !strings.Contains(about[0], tc.where+` "degub"`) || !strings.Contains(about[0], "info") {
				t.Errorf("want one warning naming %s %q and the level used, info; got %q (all warnings: %q)", tc.where, "degub", about, warnings)
			}
		})
	}
}
