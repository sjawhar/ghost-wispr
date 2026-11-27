package config

import (
	"os"
	"slices"
	"strings"
	"testing"
)

// localSettingsCanary is a value shaped like a provider key. Each file below
// holds it where a YAML error can quote it: as a value that does not fit its
// setting, as a key, as an anchor's name, or on a line that does not parse.
const localSettingsCanary = "sk-ghost-canary-4f2a9e"

// holdsCanary says whether text holds the canary or its first seven bytes,
// which is all of a value longer than ten bytes that yaml.v3 quotes in a type
// error ("`sk-ghos...`"): a check for the whole canary alone passes on that.
func holdsCanary(text string) bool {
	return strings.Contains(text, localSettingsCanary[:7])
}

// TestALocalSettingsErrorNamesNoValueOrKeyOfTheFile fails if the warning, the
// refusal of a save, or the store's report of a local settings file that the
// start did not apply holds a value or a key of that file, or if any of the
// three does not give the fixed description of the fault: its kind and its
// line. GET /api/config serves the report to any client that reaches the
// recorder, /api/status serves the warning, and a refused save returns the
// refusal, while a YAML error quotes the value or key it failed on, a
// provider's auth included (GPT-6.1 Sol's review of PR #201, round 2).
func TestALocalSettingsErrorNamesNoValueOrKeyOfTheFile(t *testing.T) {
	for _, tc := range []struct {
		name, local, want string
	}{
		{"a scalar where a provider's auth goes", "silence_timeout: 45s\nproviders:\n  deepgram:\n    auth: " + localSettingsCanary + "\n",
			"a value that does not fit its setting at line 4"},
		{"a text where a provider's auth takes a boolean", "providers:\n  deepgram:\n    auth:\n      read_only: " + localSettingsCanary + "\n",
			"a value that does not fit its setting at line 4"},
		{"a text where a number goes", "silence_timeout: 45s\ngc_max_age_days: " + localSettingsCanary + "\n",
			"a value that does not fit its setting at line 2"},
		{"a null on a key of a provider's auth", "providers:\n  deepgram:\n    auth:\n      " + localSettingsCanary + ": null\n",
			"a null at line 4 that deletes no entry: a null deletes only an entry of providers, summarization.presets or a provider's labels"},
		{"a key given twice", "silence_timeout: 45s\n" + localSettingsCanary + ": 1\n" + localSettingsCanary + ": 2\n",
			"a key given twice at line 3"},
		{"an anchor that no node defines", "silence_timeout: *" + localSettingsCanary + "\n",
			"not valid YAML"},
		{"a tab on the line of the value", "providers:\n\tdeepgram: " + localSettingsCanary + "\n",
			"not valid YAML near line 2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			path := writeConfig(t, "silence_timeout: 30s\n")
			localPath := LocalSettingsPath(path)
			if err := os.WriteFile(localPath, []byte(tc.local), 0o644); err != nil {
				t.Fatal(err)
			}
			store, warnings, err := NewStore(path)
			if err != nil {
				t.Fatalf("NewStore: %v", err)
			}
			var warning string
			for _, w := range warnings {
				if strings.HasPrefix(w, localPath+": ") {
					warning = w
				}
			}
			_, _, report := store.GetWithLocalSettings()
			if report.Err == nil {
				t.Fatalf("control: the store applied the file %q, so this case shows nothing", tc.local)
			}
			refusal := store.Update(func(c *Config) { c.GCMaxAgeDays = 3 })
			if refusal == nil {
				t.Fatal("control: a save over the file that the start did not apply was not refused")
			}
			for _, output := range []struct {
				name, text, want string
				whole            bool
			}{
				{"the warning", warning, localPath + ": " + tc.want + "; settings saved on the page are not applied", true},
				{"the report", report.Err.Error(), tc.want, true},
				{"the refusal", refusal.Error(), "did not apply " + localPath + " at start (" + tc.want + ")", false},
			} {
				if holdsCanary(output.text) {
					t.Errorf("%s holds a value or key of the file: %q", output.name, output.text)
				}
				if output.whole && output.text != output.want || !strings.Contains(output.text, output.want) {
					t.Errorf("%s is %q, want %q", output.name, output.text, output.want)
				}
			}
		})
	}
}

// TestASaveOverALocalSettingsFileEditedSinceTheStartNamesNoValueOfIt fails if
// a save whose local settings file a person changed by hand since the start,
// so that it no longer parses, returns a value or a key of that file, or does
// not say which line: the page shows the save's error to whoever pressed Save.
func TestASaveOverALocalSettingsFileEditedSinceTheStartNamesNoValueOfIt(t *testing.T) {
	clearEnv(t)
	path := writeConfig(t, "silence_timeout: 30s\n")
	localPath := LocalSettingsPath(path)
	if err := os.WriteFile(localPath, []byte("silence_timeout: 45s\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store, _, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, report := store.GetWithLocalSettings(); report.Err != nil {
		t.Fatalf("control: the start did not apply the file: %v", report.Err)
	}
	edited := "silence_timeout: 45s\n" + localSettingsCanary + ": 1\n" + localSettingsCanary + ": 2\n"
	if err := os.WriteFile(localPath, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	err = store.Update(func(c *Config) { c.GCMaxAgeDays = 3 })
	if err == nil {
		t.Fatal("control: a save over a file that no longer parses succeeded")
	}
	if holdsCanary(err.Error()) {
		t.Errorf("the save's error holds a key of the file: %q", err)
	}
	if want := "read " + localPath + ": a key given twice at line 3"; !strings.Contains(err.Error(), want) {
		t.Errorf("the save's error is %q, want it to hold %q", err, want)
	}
}

// TestALocalSettingsFileThatCannotBeReadSaysSo fails if a local settings
// path that cannot be read, here a directory, at the start or at a save gives
// anything but the fixed description: the save read the file with no
// description, so its error was the os error's text.
func TestALocalSettingsFileThatCannotBeReadSaysSo(t *testing.T) {
	const want = "cannot be read: it is a directory"
	clearEnv(t)
	path := writeConfig(t, "silence_timeout: 30s\n")
	localPath := LocalSettingsPath(path)
	if err := os.Mkdir(localPath, 0o755); err != nil {
		t.Fatal(err)
	}
	store, warnings, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, report := store.GetWithLocalSettings(); report.Err == nil || report.Err.Error() != want {
		t.Errorf("the report of a local settings path that is a directory is %v, want %q", report.Err, want)
	}
	if w := localPath + ": " + want + "; settings saved on the page are not applied"; !slices.Contains(warnings, w) {
		t.Errorf("warnings %q do not hold %q", warnings, w)
	}

	if err := os.Remove(localPath); err != nil {
		t.Fatal(err)
	}
	store, _, err = NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(localPath, 0o755); err != nil {
		t.Fatal(err)
	}
	err = store.Update(func(c *Config) { c.GCMaxAgeDays = 3 })
	if err == nil {
		t.Fatal("control: a save whose local settings path became a directory succeeded")
	}
	if got, wantSave := err.Error(), "read "+localPath+": "+want; got != wantSave {
		t.Errorf("the save's error is %q, want %q", got, wantSave)
	}
}
