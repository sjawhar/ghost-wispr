package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
)

// LocalSettingsPath is the file that holds the settings saved on the page,
// beside the release config: ghost-wispr.yaml gives ghost-wispr.local.yaml.
// A name with no .yaml suffix gets .local at its end.
func LocalSettingsPath(configPath string) string {
	dir, base := filepath.Dir(configPath), filepath.Base(configPath)
	if name, ok := strings.CutSuffix(base, ".yaml"); ok {
		return filepath.Join(dir, name+".local.yaml")
	}
	return configPath + ".local"
}

// readSettingsFile reads one settings file. A file that does not exist is
// not an error.
func readSettingsFile(path string) (data []byte, found bool, err error) {
	data, err = os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

// applyLocalSettings returns the release config's YAML with the local
// settings file's values over it. When the local settings file cannot be
// read or parsed, it returns the release config's YAML and a warning.
func applyLocalSettings(configPath string, releaseData []byte, releaseFound bool) (data []byte, found bool, warnings []string, err error) {
	localPath := LocalSettingsPath(configPath)
	localData, localFound, err := readSettingsFile(localPath)
	if err != nil {
		return releaseData, releaseFound, []string{localSettingsWarning(localPath, err)}, nil
	}
	if !localFound {
		return releaseData, releaseFound, nil, nil
	}
	local, err := parseLocalSettings(localData)
	if err != nil {
		return releaseData, releaseFound, []string{localSettingsWarning(localPath, err)}, nil
	}

	release := make(map[string]any)
	if releaseFound {
		if release, err = yamlSettings(releaseData); err != nil {
			return nil, false, nil, fmt.Errorf("parse config file: %w", err)
		}
	}
	merged, err := yaml.Marshal(mergeSettings(release, local))
	if err != nil {
		return nil, false, nil, fmt.Errorf("merge local settings: %w", err)
	}
	return merged, true, nil, nil
}

func localSettingsWarning(path string, err error) string {
	return fmt.Sprintf("%s: %v; settings saved on the page are not applied", path, err)
}

// parseLocalSettings parses a local settings file, and refuses one whose
// values do not fit the config, so that the release config stays in effect.
func parseLocalSettings(data []byte) (map[string]any, error) {
	settings, err := yamlSettings(data)
	if err != nil {
		return nil, err
	}
	var typed Config
	if err := yaml.Unmarshal(data, &typed); err != nil {
		return nil, err
	}
	return settings, nil
}

func yamlSettings(data []byte) (map[string]any, error) {
	var settings map[string]any
	if err := yaml.Unmarshal(data, &settings); err != nil {
		return nil, err
	}
	if settings == nil {
		settings = make(map[string]any)
	}
	return settings, nil
}

// mergeSettings puts the local values over the release values. Maps merge
// key by key. A null local value removes the key, as when a summary preset
// was deleted on the page. Each other local value, a list included, replaces
// the release value.
func mergeSettings(release, local map[string]any) map[string]any {
	for key, localValue := range local {
		if localValue == nil {
			delete(release, key)
			continue
		}
		if localMap, ok := localValue.(map[string]any); ok {
			releaseMap, ok := release[key].(map[string]any)
			if !ok {
				releaseMap = make(map[string]any)
			}
			release[key] = mergeSettings(releaseMap, localMap)
			continue
		}
		release[key] = localValue
	}
	return release
}

// wholeEntryMaps are the maps of struct entries, by their path in the config
// file, whose entries a save writes whole. yaml.v3 decodes each entry of such
// a map into a new zero struct, not over the entry the defaults hold, so an
// entry written in part, such as a field of the built-in default preset,
// would lose its other fields at the next start. Provider entries stay
// partial: no settings save changes one, since a save of provider_keys goes
// to .env.
var wholeEntryMaps = map[string]bool{"summarization.presets": true}

// settingsDifference returns the values of current that differ from release,
// in the form mergeSettings reads: a map holds only its keys that differ, but
// an entry of a wholeEntryMaps map that differs is whole, and a key that
// current does not have is null.
func settingsDifference(current, release map[string]any) map[string]any {
	return differenceAt("", current, release)
}

func differenceAt(path string, current, release map[string]any) map[string]any {
	whole := wholeEntryMaps[path]
	difference := make(map[string]any)
	for key, currentValue := range current {
		releaseValue, inRelease := release[key]
		currentMap, currentIsMap := currentValue.(map[string]any)
		releaseMap, releaseIsMap := releaseValue.(map[string]any)
		switch {
		case currentIsMap && releaseIsMap && !whole:
			keyPath := key
			if path != "" {
				keyPath = path + "." + key
			}
			if nested := differenceAt(keyPath, currentMap, releaseMap); len(nested) != 0 {
				difference[key] = nested
			}
		case !inRelease:
			if currentValue != nil {
				difference[key] = currentValue
			}
		case !reflect.DeepEqual(currentValue, releaseValue):
			difference[key] = currentValue
		}
	}
	for key := range release {
		if _, inCurrent := current[key]; !inCurrent {
			difference[key] = nil
		}
	}
	return difference
}

// configSettings is cfg as the YAML map a config file holds.
func configSettings(cfg *Config) (map[string]any, error) {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	return yamlSettings(data)
}

// localSettingsFile is the local settings file as it was before a save, to
// put back when the save fails after it wrote the file.
type localSettingsFile struct {
	path  string
	data  []byte
	found bool
}

func readLocalSettingsFile(configPath string) (localSettingsFile, error) {
	path := LocalSettingsPath(configPath)
	data, found, err := readSettingsFile(path)
	return localSettingsFile{path: path, data: data, found: found}, err
}

func (f localSettingsFile) restore() error {
	if !f.found {
		return removeSettingsFile(f.path)
	}
	return atomicWriteFile(f.path, f.data, 0o644)
}

func removeSettingsFile(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// writeLocalSettings writes the settings that differ from the release config
// the store loaded to the local settings file, with logLevel as the saved
// log_level. It never writes the release config, and never reads it again: a
// release file replaced under a running store would make each setting the new
// release changed differ, and the save would keep the old release's value.
// When no setting differs, no local settings file is left.
func (s *Store) writeLocalSettings(logLevel string) error {
	written := s.cfg
	written.LogLevel = logLevel
	current, err := configSettings(&written)
	if err != nil {
		return err
	}

	path := LocalSettingsPath(s.path)
	local := settingsDifference(current, s.releaseSettings)
	if len(local) == 0 {
		return removeSettingsFile(path)
	}
	data, err := yaml.Marshal(local)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return atomicWriteFile(path, data, 0o644)
}
