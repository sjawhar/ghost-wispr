package config

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
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

// applyLocalSettings merges the local settings file at localPath over the
// effective release config, baseData, and over the release file as written,
// releaseData, and returns both merged configs. When the file cannot be read
// or parsed, or a value of it does not fit its setting, it returns baseData
// and releaseData, and why it did not apply the file.
func applyLocalSettings(localPath string, baseData, releaseData []byte) (data, fileData []byte, err error) {
	localData, found, err := readSettingsFile(localPath)
	if err == nil && !found {
		return baseData, releaseData, nil
	}
	if err == nil {
		data, fileData, err = mergeLocalSettings(localData, baseData, releaseData)
	}
	if err != nil {
		return baseData, releaseData, err
	}
	return data, fileData, nil
}

// mergeLocalSettings merges the local settings file's YAML over baseData, for
// the running config, and over releaseData, for the warnings about a removed
// setting. It first decodes the local settings file alone: a value that does
// not fit its setting fails there, with the local file's own line. It then
// refuses a null anywhere but on an entry of a map setting. Each value of the
// merged config is then a value of baseData or of the local file at the same
// key, so the merged config decodes.
func mergeLocalSettings(localData, baseData, releaseData []byte) (data, fileData []byte, err error) {
	var typed Config
	if err := yaml.Unmarshal(localData, &typed); err != nil {
		return nil, nil, err
	}
	local, err := yamlSettings(localData)
	if err != nil {
		return nil, nil, err
	}
	if err := checkNulls(nil, local); err != nil {
		return nil, nil, err
	}
	base, err := yamlSettings(baseData)
	if err != nil {
		return nil, nil, err
	}
	release, err := yamlSettings(releaseData)
	if err != nil {
		return nil, nil, err
	}
	if data, err = yaml.Marshal(mergeSettings(base, local)); err != nil {
		return nil, nil, err
	}
	if fileData, err = yaml.Marshal(mergeSettings(release, local)); err != nil {
		return nil, nil, err
	}
	return data, fileData, nil
}

func localSettingsWarning(path string, err error) string {
	return fmt.Sprintf("%s: %v; settings saved on the page are not applied", path, err)
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

// mergeSettings puts the local values over the release values, in release.
// Maps merge key by key, a summary preset's fields too. A null local value
// removes the key, as when a preset was deleted on the page, the built-in
// default preset included. Each other local value, a list included, replaces
// the release value. It does not change local.
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

// isMapSetting says whether the setting at path, by its keys in a config
// file, is a map whose entries are names a person chooses: providers,
// summarization.presets and a provider's labels. Such an entry can be
// deleted, and the local settings file deletes it with a null. Every other
// mapping in a config file is a struct, whose keys are its fields: a null
// there would zero the whole struct.
func isMapSetting(path []string) bool {
	switch len(path) {
	case 1:
		return path[0] == "providers"
	case 2:
		return path[0] == "summarization" && path[1] == "presets"
	case 3:
		return path[0] == "providers" && path[2] == "labels"
	}
	return false
}

// checkNulls refuses a null in settings, at path, anywhere but on an entry of
// a map setting.
func checkNulls(path []string, settings map[string]any) error {
	for _, key := range slices.Sorted(maps.Keys(settings)) {
		child := append(slices.Clip(path), key)
		switch value := settings[key].(type) {
		case nil:
			if !isMapSetting(path) {
				return fmt.Errorf("%s: null deletes only an entry of providers, summarization.presets or a provider's labels", strings.Join(child, "."))
			}
		case map[string]any:
			if err := checkNulls(child, value); err != nil {
				return err
			}
		}
	}
	return nil
}

// settingsDifference returns the values of current that differ from release,
// in the form mergeSettings reads: a map holds only its keys that differ, a
// summary preset's fields too. An entry of a map setting that current does not
// have is null. A struct field that current does not have, because the config
// omits it when empty, holds its zero value.
func settingsDifference(current, release map[string]any) map[string]any {
	return differenceAt(nil, current, release)
}

func differenceAt(path []string, current, release map[string]any) map[string]any {
	difference := make(map[string]any)
	for key, currentValue := range current {
		releaseValue, inRelease := release[key]
		currentMap, currentIsMap := currentValue.(map[string]any)
		releaseMap, releaseIsMap := releaseValue.(map[string]any)
		switch {
		case currentIsMap && releaseIsMap:
			if nested := differenceAt(append(slices.Clip(path), key), currentMap, releaseMap); len(nested) != 0 {
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
	for key, releaseValue := range release {
		if _, inCurrent := current[key]; inCurrent || releaseValue == nil {
			continue
		}
		if isMapSetting(path) {
			difference[key] = nil
			continue
		}
		if releaseMap, ok := releaseValue.(map[string]any); ok {
			if nested := differenceAt(append(slices.Clip(path), key), map[string]any{}, releaseMap); len(nested) != 0 {
				difference[key] = nested
			}
			continue
		}
		difference[key] = reflect.Zero(reflect.TypeOf(releaseValue)).Interface()
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
