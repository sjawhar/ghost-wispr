package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// loglineMark checks that a parsed log line carries its mark as Cloud
// Logging's user labels, the object under "logging.googleapis.com/labels"
// that Cloud Run's logging agent moves into LogEntry.labels, for component,
// and not as the top-level field a log view's filter cannot read.
func loglineMark(line map[string]any, component string) error {
	labels, _ := line["logging.googleapis.com/labels"].(map[string]any)
	if len(labels) != 2 || labels["ghost_wispr_marked"] != "true" || labels["ghost_wispr_component"] != component {
		return fmt.Errorf("labels %v, want ghost_wispr_marked=true and ghost_wispr_component=%s alone", line["logging.googleapis.com/labels"], component)
	}
	if _, ok := line["ghost_wispr_component"]; ok {
		return errors.New("the line keeps the top-level ghost_wispr_component")
	}
	return nil
}

// The watcher's alert counts WATCHER-PASS at severity NOTICE, so a NOTICE
// line must read NOTICE, not slog's "INFO+2", and every line must carry its
// mark as labels for the log exclusion and a log view to keep it.
func TestTheLogLineMapsNoticeToItsSeverity(t *testing.T) {
	var buf bytes.Buffer
	log := newLogger(&buf, "watcher", slog.LevelDebug)
	log.Debug("D", "n", 1)
	log.Info("I", "n", 2)
	log.Log(t.Context(), LevelNotice, "N", "n", 3)
	log.Warn("W", "n", 4)
	log.Error("E", "n", 5)

	want := []struct {
		severity, message string
		n                 float64
	}{
		{"DEBUG", "D", 1},
		{"INFO", "I", 2},
		{"NOTICE", "N", 3},
		{"WARNING", "W", 4},
		{"ERROR", "E", 5},
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != len(want) {
		t.Fatalf("got %d lines, want %d:\n%s", len(lines), len(want), buf.String())
	}
	for i, line := range lines {
		var got map[string]any
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatalf("line %d is not one JSON object: %v: %s", i, err, line)
		}
		if got["severity"] != want[i].severity {
			t.Errorf("line %d: severity %v, want %s", i, got["severity"], want[i].severity)
		}
		if got["message"] != want[i].message {
			t.Errorf("line %d: message %v, want %s", i, got["message"], want[i].message)
		}
		if err := loglineMark(got, "watcher"); err != nil {
			t.Errorf("line %d: %v", i, err)
		}
		if got["n"] != want[i].n {
			t.Errorf("line %d: attribute n %v, want %v", i, got["n"], want[i].n)
		}
		for _, key := range []string{"level", "msg"} {
			if _, ok := got[key]; ok {
				t.Errorf("line %d keeps slog's own key %q: %s", i, key, line)
			}
		}
	}
}
