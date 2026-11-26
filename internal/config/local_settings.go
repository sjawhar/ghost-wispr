package config

import (
	"fmt"
	"maps"
	"math"
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
// releaseData, and returns both merged configs and the settings it took from
// the file, local. With no file, local is nil. When the file cannot be read
// or parsed, or a value of it does not fit its setting, it returns baseData
// and releaseData, no settings, and why it did not apply the file.
func applyLocalSettings(localPath string, baseData, releaseData []byte) (data, fileData []byte, local map[string]any, err error) {
	localData, found, err := readSettingsFile(localPath)
	if err == nil && !found {
		return baseData, releaseData, nil, nil
	}
	if err == nil {
		data, fileData, local, err = mergeLocalSettings(localData, baseData, releaseData)
	}
	if err != nil {
		return baseData, releaseData, nil, err
	}
	return data, fileData, local, nil
}

// mergeLocalSettings merges the local settings file's YAML over baseData, for
// the running config, and over releaseData, for the warnings about a removed
// setting, and returns the file's settings too. It first decodes the local
// settings file alone: a value that does not fit its setting fails there,
// with the local file's own line. It then refuses a null anywhere but on an
// entry of a map setting. Each value of the merged config is then a value of
// baseData or of the local file at the same key, so the merged config decodes.
func mergeLocalSettings(localData, baseData, releaseData []byte) (data, fileData []byte, local map[string]any, err error) {
	var typed Config
	if err := yaml.Unmarshal(localData, &typed); err != nil {
		return nil, nil, nil, err
	}
	local, err = yamlSettings(localData)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := checkNulls(nil, local); err != nil {
		return nil, nil, nil, err
	}
	base, err := yamlSettings(baseData)
	if err != nil {
		return nil, nil, nil, err
	}
	release, err := yamlSettings(releaseData)
	if err != nil {
		return nil, nil, nil, err
	}
	if data, err = yaml.Marshal(mergeSettings(base, local)); err != nil {
		return nil, nil, nil, err
	}
	if fileData, err = yaml.Marshal(mergeSettings(release, local)); err != nil {
		return nil, nil, nil, err
	}
	return data, fileData, local, nil
}

// AppliedLocalSettings is the local settings file as the running config holds
// it, which GET /api/config reports, so that a reader such as
// deploy/pi/check-deploy.sh need not read the file's YAML itself.
type AppliedLocalSettings struct {
	// Settings are the settings the running config took from the local
	// settings file, as that file writes them: those the start applied, as
	// each page save has changed them since. A null is an entry of a map
	// setting that they delete, such as a provider. Each provider's auth is
	// left out. Nil when the store applied no local settings file: there is
	// none, or the start did not apply it (Err).
	Settings map[string]any
	// Err is why the start did not apply the local settings file, or nil.
	Err error
}

// reportedSettings is a copy of settings for AppliedLocalSettings, with each
// provider's auth left out, and each value in a form encoding/json writes: a
// mapping YAML decoded with keys that are not all strings gets string keys,
// and a float JSON cannot hold (.inf, .nan) becomes its text.
func reportedSettings(settings map[string]any) map[string]any {
	if settings == nil {
		return nil
	}
	reported, _ := reportedValue(settings).(map[string]any)
	if providers, ok := reported["providers"].(map[string]any); ok {
		for _, provider := range providers {
			if fields, ok := provider.(map[string]any); ok {
				delete(fields, "auth")
			}
		}
	}
	return reported
}

func reportedValue(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[key] = reportedValue(item)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[fmt.Sprint(key)] = reportedValue(item)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = reportedValue(item)
		}
		return out
	case float64:
		if math.IsInf(v, 0) || math.IsNaN(v) {
			return fmt.Sprint(v)
		}
		return v
	default:
		return v
	}
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

// settingsDifference returns the values of current that differ from previous,
// in the form mergeSettings reads: a map holds only its keys that differ, a
// summary preset's fields too. An entry of a map setting that current does not
// have is null. A struct field that current does not have, because the config
// omits it when empty, holds its zero value.
func settingsDifference(current, previous map[string]any) map[string]any {
	return differenceAt(nil, current, previous)
}

func differenceAt(path []string, current, previous map[string]any) map[string]any {
	difference := make(map[string]any)
	for key, currentValue := range current {
		previousValue, inPrevious := previous[key]
		currentMap, currentIsMap := currentValue.(map[string]any)
		previousMap, previousIsMap := previousValue.(map[string]any)
		switch {
		case currentIsMap && previousIsMap:
			if nested := differenceAt(append(slices.Clip(path), key), currentMap, previousMap); len(nested) != 0 {
				difference[key] = nested
			}
		case !inPrevious:
			if currentValue != nil {
				difference[key] = currentValue
			}
		case !reflect.DeepEqual(currentValue, previousValue):
			difference[key] = currentValue
		}
	}
	for key, previousValue := range previous {
		if _, inCurrent := current[key]; inCurrent || previousValue == nil {
			continue
		}
		if isMapSetting(path) {
			difference[key] = nil
			continue
		}
		if previousMap, ok := previousValue.(map[string]any); ok {
			if nested := differenceAt(append(slices.Clip(path), key), map[string]any{}, previousMap); len(nested) != 0 {
				difference[key] = nested
			}
			continue
		}
		difference[key] = reflect.Zero(reflect.TypeOf(previousValue)).Interface()
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

// writeLocalSettings writes the settings that this change set, from prev to
// the running config, into the local settings file as it was before the
// change, local. It writes no other setting: a value that an environment
// variable gives is the same before and after the change, so a save never
// writes it, and the release config's value applies again once the variable
// is removed. The other settings of the local settings file stay. A setting
// set back to the release config's value leaves the file, and the file is
// removed when no setting is left. It never writes the release config, and
// compares with the release config the store loaded, not the file on disk. It
// returns the changes it wrote, as settingsDifference gives them, or none when
// the change set no setting.
func (s *Store) writeLocalSettings(prev *Config, local localSettingsFile) (map[string]any, error) {
	before, err := configSettings(prev)
	if err != nil {
		return nil, err
	}
	after, err := configSettings(&s.cfg)
	if err != nil {
		return nil, err
	}
	changes := settingsDifference(after, before)
	if len(changes) == 0 {
		return nil, nil
	}
	saved := make(map[string]any)
	if local.found {
		if saved, err = yamlSettings(local.data); err != nil {
			return nil, fmt.Errorf("read %s: %w", local.path, err)
		}
	}
	applyChanges(nil, saved, changes, s.releaseSettings)
	if len(saved) == 0 {
		return changes, removeSettingsFile(local.path)
	}
	data, err := yaml.Marshal(saved)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(local.path), 0o755); err != nil {
		return nil, err
	}
	return changes, atomicWriteFile(local.path, data, 0o644)
}

// applyLocalChanges puts changes, as writeLocalSettings wrote them into the
// local settings file, into the settings the running config holds from that
// file, as applyChanges puts them into the file's.
func (s *Store) applyLocalChanges(changes map[string]any) {
	if len(changes) == 0 {
		return
	}
	if s.localSettings == nil {
		s.localSettings = make(map[string]any)
	}
	applyChanges(nil, s.localSettings, changes, s.releaseSettings)
	if len(s.localSettings) == 0 {
		s.localSettings = nil
	}
}

// applyChanges writes changes, as settingsDifference gives them, into saved,
// the local settings file's map at path. release is the release config's map
// at the same path. A changed value equal to the release config's leaves
// saved, and a map left empty leaves it too. An entry of a map setting that
// the release config does not have, such as a preset made on the page, lives
// only in saved: its fields are written as they are, and it stays whole.
func applyChanges(path []string, saved, changes, release map[string]any) {
	for key, change := range changes {
		releaseValue, inRelease := release[key]
		changeMap, isMap := change.(map[string]any)
		if isMap && isMapSetting(path) && !inRelease {
			entry, _ := saved[key].(map[string]any)
			if entry == nil {
				entry = make(map[string]any)
			}
			saved[key] = mergeSettings(entry, changeMap)
			continue
		}
		if isMap {
			savedMap, _ := saved[key].(map[string]any)
			if savedMap == nil {
				savedMap = make(map[string]any)
			}
			releaseMap, _ := releaseValue.(map[string]any)
			applyChanges(append(slices.Clip(path), key), savedMap, changeMap, releaseMap)
			if len(savedMap) == 0 {
				delete(saved, key)
			} else {
				saved[key] = savedMap
			}
			continue
		}
		if equalsRelease(path, change, releaseValue, inRelease) {
			delete(saved, key)
		} else {
			saved[key] = change
		}
	}
}

// equalsRelease says whether value, at a key below path, is the release
// config's. A key the release config does not have is a deleted entry when it
// is an entry of a map setting, and otherwise a struct field the config omits
// when it holds its zero value.
func equalsRelease(path []string, value, releaseValue any, inRelease bool) bool {
	if inRelease {
		return reflect.DeepEqual(value, releaseValue)
	}
	if value == nil {
		return true
	}
	return !isMapSetting(path) && reflect.ValueOf(value).IsZero()
}
