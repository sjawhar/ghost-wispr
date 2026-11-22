package config

import "testing"

// TestPathFromEnvironmentIsTheRecordersConfigFile fails if the config file a
// command loads is not GHOST_WISPR_CONFIG when it is set, else
// ghost-wispr.yaml in the working directory, as the recorder loads it.
func TestPathFromEnvironmentIsTheRecordersConfigFile(t *testing.T) {
	t.Setenv(EnvPrefix+"CONFIG", "/srv/example/unit.yaml")
	if got := PathFromEnvironment(); got != "/srv/example/unit.yaml" {
		t.Errorf("with %sCONFIG set, config path %q, want /srv/example/unit.yaml", EnvPrefix, got)
	}
	t.Setenv(EnvPrefix+"CONFIG", "")
	if got := PathFromEnvironment(); got != "ghost-wispr.yaml" {
		t.Errorf("with %sCONFIG unset, config path %q, want ghost-wispr.yaml", EnvPrefix, got)
	}
}
