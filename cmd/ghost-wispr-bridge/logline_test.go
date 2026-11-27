package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// The watcher's alert counts WATCHER-PASS at severity NOTICE, so a NOTICE
// line must read NOTICE, not slog's "INFO+2", and every line must carry its
// component for the log exclusion to keep it.
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
		if got["ghost_wispr_component"] != "watcher" {
			t.Errorf("line %d: ghost_wispr_component %v, want watcher", i, got["ghost_wispr_component"])
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
