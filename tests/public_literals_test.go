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
// project's number, and its cloud resources, the Pi's Secret Manager secrets,
// its service account's unique ID and its Tailscale client id among them. Each
// is split so this file does not hold the name it looks for.
var companyLiterals = []string{
	"theo" + "rem",
	"tail12" + "ec1c",
	"agent" + "-infra",
	"8052409" + "12354",
	"envoy-ghostwispr" + "-signing-secret",
	"ghost-wispr-device" + "-certificate",
	"operator-only-ghost" + "-wispr",
	"1148810755" + "58003314337",
	"TWbBfz7fqc11" + "CNTRL-kX7aTA7yJt11CNTRL",
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

var companyLiteral = regexp.MustCompile(`(?i)` + strings.Join(companyLiterals, "|"))

// companyLiteralHits returns "file:line: text" for each line of data, the
// content of the tracked file file, that names the company or one of its
// resources, and nothing for a file of piDeployment.
func companyLiteralHits(file, data string) []string {
	if piDeployment[file] {
		return nil
	}
	var hits []string
	for n, line := range strings.Split(data, "\n") {
		if companyLiteral.MatchString(line) {
			hits = append(hits, fmt.Sprintf("%s:%d: %s", file, n+1, strings.TrimSpace(line)))
		}
	}
	return hits
}

// TestNoCompanyLiteralsOutsideThePiDeployment fails if a tracked file other
// than the office Pi's config, deployment settings and runbook names the
// company that runs that Pi or one of its resources: the product code, its
// docs and its tests use neutral names (example-project and the like).
func TestNoCompanyLiteralsOutsideThePiDeployment(t *testing.T) {
	// The control, before the tree is read: each of the Pi's settings that
	// pi.env holds and no other file may, planted in a file outside the Pi's
	// deployment, is found there, on its line, and passes in each file of it.
	for _, planted := range []struct{ name, line string }{
		{"the Deepgram key's secret", "PI_DEEPGRAM_SECRET=operator-only-ghost" + "-wispr-deepgram-key"},
		{"the service account's unique ID", "PI_SERVICE_ACCOUNT_UNIQUE_ID=1148810755" + "58003314337"},
		{"the Tailscale client id", "PI_TAILNET_CLIENT_ID=TWbBfz7fqc11" + "CNTRL-kX7aTA7yJt11CNTRL"},
	} {
		t.Run(planted.name, func(t *testing.T) {
			data := "# A file of the repository.\n" + planted.line + "\n"
			want := "README.md:2: " + planted.line
			if hits := companyLiteralHits("README.md", data); len(hits) != 1 || hits[0] != want {
				t.Errorf("planted in README.md, the check found %q, want [%q]", hits, want)
			}
			for file := range piDeployment {
				if hits := companyLiteralHits(file, data); len(hits) != 0 {
					t.Errorf("in %s, which may name it, the check found %q", file, hits)
				}
			}
		})
	}

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
		hits = append(hits, companyLiteralHits(file, string(data))...)
	}
	if len(hits) > 0 {
		t.Errorf("%d lines in %d files read name the office Pi's company outside its config, deployment settings and runbook:\n%s",
			len(hits), read, strings.Join(hits, "\n"))
	}
}
