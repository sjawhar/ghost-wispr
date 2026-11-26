package tests

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// companyLiterals are the names that tie this public repository to the company
// that runs the office Pi: the company itself (its name, its domain and its
// GitHub organization), its tailnet, its infrastructure repository, its Google
// project's number, and its cloud resources, the Pi's Secret Manager secrets
// among them. Each is split so this file does not hold the name it looks for.
var companyLiterals = []string{
	"theo" + "rem",
	"tail12" + "ec1c",
	"agent" + "-infra",
	"8052409" + "12354",
	"envoy-ghostwispr" + "-signing-secret",
	"ghost-wispr-device" + "-certificate",
	"operator-only-ghost" + "-wispr",
	"envoy-webhook" + "-relay",
	"envoy" + "-hub",
	"envoy" + "-cluster",
	"envoy" + "-host",
	"tag:hind" + "sight",
	"hindsight" + "-control-plane",
	"ghostWispr" + "AccountUniqueId",
	"ghostWispr" + "TailnetClientId",
	"Pulumi" + `\.prod`,
}

// piDeployment are the files that describe the office Pi itself, and so may
// name its company: its app config, its deployment settings and its runbook.
var piDeployment = map[string]bool{
	"deploy/pi/files/ghost-wispr.yaml": true,
	"deploy/pi/files/pi.env":           true,
	"docs/pi-setup.md":                 true,
}

// TestNoCompanyLiteralsOutsideThePiDeployment fails if a tracked file other
// than the office Pi's config, deployment settings and runbook names the
// company that runs that Pi or one of its resources: the product code, its
// docs and its tests use neutral names (example-project and the like).
func TestNoCompanyLiteralsOutsideThePiDeployment(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate this test")
	}
	repo := filepath.Dir(filepath.Dir(thisFile))
	listed, err := exec.Command("git", "-C", repo, "ls-files", "-z").Output()
	if err != nil {
		t.Fatalf("list the tracked files: %v", err)
	}
	files := strings.Split(strings.TrimSuffix(string(listed), "\x00"), "\x00")
	// A check that read no file has not passed: it could not see the tree.
	if len(files) < 100 {
		t.Fatalf("git ls-files listed %d files in %s, too few for this repository; the check cannot see the tree", len(files), repo)
	}
	literal := regexp.MustCompile(`(?i)` + strings.Join(companyLiterals, "|"))
	var hits []string
	read := 0
	for _, file := range files {
		if piDeployment[file] {
			continue
		}
		data, err := os.ReadFile(filepath.Join(repo, file))
		if err != nil {
			t.Fatalf("read tracked file %s: %v", file, err)
		}
		read++
		for n, line := range strings.Split(string(data), "\n") {
			if literal.MatchString(line) {
				hits = append(hits, fmt.Sprintf("%s:%d: %s", file, n+1, strings.TrimSpace(line)))
			}
		}
	}
	if len(hits) > 0 {
		t.Errorf("%d lines in %d files read name the office Pi's company outside its config, deployment settings and runbook:\n%s",
			len(hits), read, strings.Join(hits, "\n"))
	}
}
