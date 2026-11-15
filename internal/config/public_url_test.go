package config

import (
	"strings"
	"testing"
)

// TestPublicURLIsTheUnitsHTTPSAddress fails if public_url keeps anything but
// an HTTPS address with no path: the web UI trusts pages at it, and Envoy
// events link sessions under it.
func TestPublicURLIsTheUnitsHTTPSAddress(t *testing.T) {
	cases := []struct {
		value, want string
		warns       bool
	}{
		{"https://ghost-wispr.example.ts.net", "https://ghost-wispr.example.ts.net", false},
		{"https://ghost-wispr.example.ts.net/", "https://ghost-wispr.example.ts.net", false},
		{"http://ghost-wispr.example.ts.net", "", true},
		{"https://ghost-wispr.example.ts.net/app", "", true},
		{"https://ghost-wispr.example.ts.net?x=1", "", true},
		{"ghost-wispr.example.ts.net", "", true},
	}
	for _, tc := range cases {
		clearEnv(t)
		t.Setenv(EnvPrefix+"PUBLIC_URL", "")
		cfg, warnings, err := Load(writeConfig(t, "public_url: "+tc.value+"\n"))
		if err != nil {
			t.Fatalf("Load(public_url %q): %v", tc.value, err)
		}
		if cfg.PublicURL != tc.want {
			t.Errorf("public_url %q loaded as %q, want %q", tc.value, cfg.PublicURL, tc.want)
		}
		warned := false
		for _, warning := range warnings {
			warned = warned || strings.Contains(warning, "public_url")
		}
		if warned != tc.warns {
			t.Errorf("public_url %q warned %t, want %t: %v", tc.value, warned, tc.warns, warnings)
		}
	}

	clearEnv(t)
	t.Setenv(EnvPrefix+"PUBLIC_URL", "https://override.example.ts.net")
	cfg, _, err := Load(writeConfig(t, "public_url: https://ghost-wispr.example.ts.net\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PublicURL != "https://override.example.ts.net" {
		t.Fatalf("%sPUBLIC_URL did not override public_url: %q", EnvPrefix, cfg.PublicURL)
	}
}
