// Package logtest captures, for tests, the records code writes through slog's
// default logger, and through the log package that slog's default carries.
package logtest

import (
	"bytes"
	"encoding/json"
	"log"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// Record is one captured log record: its level (DEBUG, INFO, WARN, ERROR),
// its message, and its attributes, as the JSON handler writes them.
type Record struct {
	Level   string
	Message string
	Attrs   map[string]any
}

// Log holds the records written since Capture.
type Log struct {
	t   testing.TB
	mu  sync.Mutex
	buf bytes.Buffer
}

// Capture makes slog's default logger write every record, at every level, to
// the returned Log until the test ends. It then restores slog's default, and
// the log package's writer and flags, which slog.SetDefault changes.
func Capture(t testing.TB) *Log {
	t.Helper()
	l := &Log{t: t}
	previous, writer, flags := slog.Default(), log.Writer(), log.Flags()
	slog.SetDefault(slog.New(slog.NewJSONHandler(l, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() {
		slog.SetDefault(previous)
		log.SetOutput(writer)
		log.SetFlags(flags)
	})
	return l
}

func (l *Log) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

// Records returns every record written so far, in order.
func (l *Log) Records() []Record {
	l.t.Helper()
	l.mu.Lock()
	text := l.buf.String()
	l.mu.Unlock()
	var records []Record
	for line := range strings.Lines(text) {
		var attrs map[string]any
		if err := json.Unmarshal([]byte(line), &attrs); err != nil {
			l.t.Fatalf("captured log line %q is not a JSON record: %v", line, err)
		}
		record := Record{Attrs: attrs}
		record.Level, _ = attrs[slog.LevelKey].(string)
		record.Message, _ = attrs[slog.MessageKey].(string)
		delete(attrs, slog.TimeKey)
		delete(attrs, slog.LevelKey)
		delete(attrs, slog.MessageKey)
		records = append(records, record)
	}
	return records
}

// At returns the records written so far at level, in order.
func (l *Log) At(level slog.Level) []Record {
	l.t.Helper()
	var at []Record
	for _, record := range l.Records() {
		if record.Level == level.String() {
			at = append(at, record)
		}
	}
	return at
}
