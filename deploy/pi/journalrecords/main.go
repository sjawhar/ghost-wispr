// Command journalrecords writes log records as ghost-wispr.service writes them to its journal:
// each one through logging.New, the logger the service writes with, at the service's level. The
// fake journalctl of deploy/pi/accept_voice_test.sh prints its records through it, so that a
// driver that reads a key the service does not write fails that test.
//
// Each line of standard input is one JSON object. {"raw": TEXT} prints TEXT as it is, as the
// service's log.Println lines before its logger is set up. Any other line is one record:
// {"time_ns": N, "level": "INFO", "msg": TEXT, "attrs": [[KEY, VALUE], ...]}, its attributes in
// that order; attrs may be left out. A line with another key, a record without time_ns, level or
// msg, or a raw line with a record's keys is an error: the command prints the lines before it and
// exits 1.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/logging"
)

// line is one line of standard input: Raw alone, or a record's TimeNS, Level and Msg, with its
// Attrs. A nil field is a key the line left out.
type line struct {
	Raw    *string     `json:"raw"`
	TimeNS *int64      `json:"time_ns"`
	Level  *slog.Level `json:"level"`
	Msg    *string     `json:"msg"`
	Attrs  [][]any     `json:"attrs"`
}

func main() {
	if err := write(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "journalrecords: %v\n", err)
		os.Exit(1)
	}
}

func write(in io.Reader, out io.Writer) error {
	// The service logs at log_level "info", its default (internal/config), which the Pi keeps.
	handler := logging.New(out, logging.ParseLevel("info")).Handler()
	decoder := json.NewDecoder(in)
	decoder.DisallowUnknownFields()
	// A number stays a json.Number, which the handler writes with the digits it was read with; a
	// float64 would lose the low digits of the service's Unix times in nanoseconds.
	decoder.UseNumber()
	// Decode to io.EOF, not while More: More is also false at a read error or a stray ']', and
	// says neither.
	for n := 1; ; n++ {
		var l line
		if err := decoder.Decode(&l); errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return fmt.Errorf("line %d: %w", n, err)
		}
		if err := l.emit(out, handler); err != nil {
			return fmt.Errorf("line %d: %w", n, err)
		}
	}
}

// emit prints l: its raw text, or its record through handler, as the service's logger does, which
// drops a record below the handler's level.
func (l line) emit(out io.Writer, handler slog.Handler) error {
	if l.Raw != nil {
		if l.TimeNS != nil || l.Level != nil || l.Msg != nil || l.Attrs != nil {
			return errors.New(`a "raw" line has a record's keys too`)
		}
		_, err := fmt.Fprintln(out, *l.Raw)
		return err
	}
	if l.TimeNS == nil || l.Level == nil || l.Msg == nil {
		return errors.New(`a record needs "time_ns", "level" and "msg"`)
	}
	ctx := context.Background()
	if !handler.Enabled(ctx, *l.Level) {
		return nil
	}
	record := slog.NewRecord(time.Unix(0, *l.TimeNS), *l.Level, *l.Msg, 0)
	for _, pair := range l.Attrs {
		if len(pair) != 2 {
			return fmt.Errorf("attribute %v is not [KEY, VALUE]", pair)
		}
		key, ok := pair[0].(string)
		if !ok {
			return fmt.Errorf("attribute %v has no string key", pair)
		}
		// A string or a bool reaches the handler as the kind the service logs it with. A number is a
		// json.Number, which the handler writes as its digits, as it writes the service's ints and
		// time.Durations (in nanoseconds).
		record.AddAttrs(slog.Any(key, pair[1]))
	}
	return handler.Handle(ctx, record)
}
