package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/sjawhar/ghost-wispr/internal/config"
)

const defaultConfigPath = "ghost-wispr.yaml"

// configPathFromEnvironment returns the configuration file the recorder and
// its subcommands load: GHOST_WISPR_CONFIG, else ghost-wispr.yaml in the
// working directory.
func configPathFromEnvironment() string {
	if configPath := os.Getenv(config.EnvPrefix + "CONFIG"); configPath != "" {
		return configPath
	}
	return defaultConfigPath
}

// loadRuntimeConfig loads configPath and the .env file beside it, as the
// recorder runs with them.
func loadRuntimeConfig(configPath string) (*config.Store, []string, error) {
	envPath := filepath.Join(filepath.Dir(configPath), ".env")
	return config.NewStoreWithEnv(configPath, envPath)
}

// databasePath returns the database file the recorder opens for cfg: db_path,
// resolved against the working directory when it is relative.
func databasePath(dbPath string) (string, error) {
	path, err := filepath.Abs(dbPath)
	if err != nil {
		return "", fmt.Errorf("resolve database path %q: %w", dbPath, err)
	}
	return path, nil
}
