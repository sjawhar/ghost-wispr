package main

import (
	"bytes"
	"os"
	"regexp"
	"testing"
)

// TestHelpListsEverySubcommand fails if ghost-wispr --help, -h or help does
// not print on stdout, with exit 0, a line that names and describes each
// subcommand: each one main runs (os.Args[1] == "…" in main.go) and the light
// commands; or if an unknown subcommand does not print the same list on
// stderr with exit 2.
func TestHelpListsEverySubcommand(t *testing.T) {
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	subcommands := []string{"light-off", "light-state"}
	named := regexp.MustCompile(`os\.Args\[1\] == "([a-z-]+)"`).FindAllSubmatch(source, -1)
	// main.go ran 4 subcommands of its own when this test was written: fewer
	// means the pattern no longer finds them, not that they are gone.
	if len(named) < 4 {
		t.Fatalf("main.go names %d subcommands by os.Args[1], want at least 4: the pattern cannot see them", len(named))
	}
	for _, m := range named {
		subcommands = append(subcommands, string(m[1]))
	}
	listsEach := func(t *testing.T, what string, out []byte) {
		t.Helper()
		for _, name := range subcommands {
			if !regexp.MustCompile(`(?m)^  ` + regexp.QuoteMeta(name) + `\b.*  +\S`).Match(out) {
				t.Errorf("%s has no line naming and describing %s:\n%s", what, name, out)
			}
		}
	}
	for _, arg := range []string{"--help", "-h", "help"} {
		var stdout, stderr bytes.Buffer
		if code := runLightCommand([]string{arg}, &stdout, &stderr); code != 0 || stderr.Len() != 0 {
			t.Errorf("ghost-wispr %s: exit %d, stderr %q; want 0 and nothing", arg, code, stderr.String())
		}
		listsEach(t, "ghost-wispr "+arg, stdout.Bytes())
	}
	var stdout, stderr bytes.Buffer
	if code := runLightCommand([]string{"unexpected"}, &stdout, &stderr); code != 2 || stdout.Len() != 0 {
		t.Errorf("an unknown subcommand: exit %d, stdout %q; want 2 and nothing", code, stdout.String())
	}
	listsEach(t, "an unknown subcommand's stderr", stderr.Bytes())
}
