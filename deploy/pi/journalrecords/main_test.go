package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

// TestARecordIsWrittenAsTheServiceLogsIt fails if a record does not come out as logging.New
// writes the service's: its message under "message", its time, and each attribute with its JSON
// type and every digit of its number.
func TestARecordIsWrittenAsTheServiceLogsIt(t *testing.T) {
	in := `{"raw": "2026/11/19 10:00:00 ghost-wispr: starting"}
{"time_ns": 1763542800000000005, "level": "WARN", "msg": "speaker underrun", "attrs": [["device", "plughw:CARD=J75,DEV=0"], ["underruns", 1], ["at_unix_ns", 1763542800000000005], ["rewind", true]]}
`
	var out bytes.Buffer
	if err := write(strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 2 || lines[0] != "2026/11/19 10:00:00 ghost-wispr: starting" {
		t.Fatalf("wrote %q, want the raw line, then one record", out.String())
	}
	decoder := json.NewDecoder(strings.NewReader(lines[1]))
	decoder.UseNumber()
	var got map[string]any
	if err := decoder.Decode(&got); err != nil {
		t.Fatalf("%v: %s", err, lines[1])
	}
	at, err := time.Parse(time.RFC3339Nano, fmt.Sprint(got["timestamp"]))
	if err != nil || at.UnixNano() != 1763542800000000005 {
		t.Errorf("timestamp %v (%v), want the record's time_ns", got["timestamp"], err)
	}
	if _, ok := got["msg"]; ok || got["message"] != "speaker underrun" || got["level"] != "WARN" {
		t.Errorf("wrote %s, want message \"speaker underrun\" at level WARN, and no msg", lines[1])
	}
	if got["device"] != "plughw:CARD=J75,DEV=0" || got["underruns"] != json.Number("1") ||
		got["at_unix_ns"] != json.Number("1763542800000000005") || got["rewind"] != true {
		t.Errorf("wrote %s, want each attribute with its type and digits", lines[1])
	}
}

// TestARecordBelowTheServiceLevelIsNotWritten fails if a record the service's logger would drop
// at its level, info, reaches the journal.
func TestARecordBelowTheServiceLevelIsNotWritten(t *testing.T) {
	var out bytes.Buffer
	if err := write(strings.NewReader(`{"time_ns": 1, "level": "DEBUG", "msg": "m"}`), &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("wrote %q, want nothing", out.String())
	}
}

// TestABadLineIsRefused fails if a line the fake journal could not mean is printed as a record, or
// the end of a cut input is taken for the end of the journal.
func TestABadLineIsRefused(t *testing.T) {
	for _, tc := range []struct{ name, in string }{
		{"a key the helper does not read", `{"time_ns": 1, "level": "INFO", "message": "voice timing"}`},
		{"no msg", `{"time_ns": 1, "level": "INFO"}`},
		{"no time_ns", `{"level": "INFO", "msg": "m"}`},
		{"no level", `{"time_ns": 1, "msg": "m"}`},
		{"a level slog does not name", `{"time_ns": 1, "level": "LOUD", "msg": "m"}`},
		{"a time_ns that is not whole", `{"time_ns": 1.5, "level": "INFO", "msg": "m"}`},
		{"a raw line with a record's key", `{"raw": "x", "level": "INFO"}`},
		{"an attribute that is no pair", `{"time_ns": 1, "level": "INFO", "msg": "m", "attrs": [["k"]]}`},
		{"an attribute key not a string", `{"time_ns": 1, "level": "INFO", "msg": "m", "attrs": [[1, "v"]]}`},
		{"an input cut in a line", `{"raw": "x"`},
		{"a stray bracket", "{\"raw\": \"x\"}\n]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := write(strings.NewReader(tc.in), io.Discard)
			if err == nil || !strings.HasPrefix(err.Error(), "line ") {
				t.Fatalf("write(%q) = %v, want an error that names the line", tc.in, err)
			}
		})
	}
}
