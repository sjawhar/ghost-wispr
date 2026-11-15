package tests

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestMakeBuildRetriesAFailedNpmInstall fails if a failed npm ci leaves an
// up-to-date prerequisite that lets the next make frontend-build skip npm ci.
func TestMakeBuildRetriesAFailedNpmInstall(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate this test")
	}
	repo := filepath.Dir(filepath.Dir(thisFile))
	work := t.TempDir()
	if err := os.Symlink(filepath.Join(repo, "Makefile"), filepath.Join(work, "Makefile")); err != nil {
		t.Fatalf("link Makefile: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(work, "web"), 0o755); err != nil {
		t.Fatalf("make web directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(work, "web", "package-lock.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write package lock: %v", err)
	}
	bin := filepath.Join(work, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatalf("make npm directory: %v", err)
	}
	log := filepath.Join(work, "npm.log")
	const npm = `#!/bin/sh
printf '%s\n' "$*" >> "$NPM_LOG"
if [ "$3" = ci ]; then
	mkdir -p web/node_modules
	if [ ! -e .npm-ci-failed ]; then
		touch .npm-ci-failed
		exit 1
	fi
fi
`
	if err := os.WriteFile(filepath.Join(bin, "npm"), []byte(npm), 0o700); err != nil {
		t.Fatalf("write fake npm: %v", err)
	}
	run := func() error {
		cmd := exec.Command("make", "frontend-build")
		cmd.Dir = work
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "NPM_LOG="+log)
		return cmd.Run()
	}
	if err := run(); err == nil {
		t.Fatal("first frontend build succeeded though npm ci was made to fail")
	}
	if err := run(); err != nil {
		t.Fatalf("second frontend build: %v", err)
	}
	calls, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("read npm calls: %v", err)
	}
	if got := strings.Count(string(calls), "--prefix web ci"); got != 2 {
		t.Fatalf("npm ci calls = %d, want 2 after the first install failed; calls: %s", got, calls)
	}
}
