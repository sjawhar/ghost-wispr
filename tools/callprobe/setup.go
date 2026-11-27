package main

import (
	_ "embed"
	"flag"
	"fmt"
	"io"
	"os/exec"
)

// setupScript is setup-linux.sh, built in so that `go run ./tools/callprobe
// setup` needs no path to it.
//
//go:embed setup-linux.sh
var setupScript string

func runSetup(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("callprobe setup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	remove := fs.Bool("remove", false, "remove the devices setup made")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintln(stderr, "usage: callprobe setup [-remove]")
		return 2
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "callprobe setup: %v\n", err)
		return 1
	}
	action := "up"
	if *remove {
		action = "down"
	}
	cmd := exec.Command(bash, "-c", setupScript, "setup-linux.sh", action)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil {
		_, _ = fmt.Fprintf(stderr, "callprobe setup: setup-linux.sh %s: %v\n", action, err)
		return 1
	}
	return 0
}
