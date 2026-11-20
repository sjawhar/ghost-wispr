package logging

import (
	"log/slog"
	"testing"
	"time"
)

// GET /api/logs?level= names a level as the OpenAPI spec does, in lower case,
// or as the log viewer does, in upper case: both select the same lines.
func TestLogSink_LevelFilterIgnoresCase(t *testing.T) {
	sink := NewLogSink(10)
	logger := New(sink, slog.LevelDebug)
	logger.Info("an info line")
	logger.Error("an error line")

	for _, level := range []string{"error", "ERROR", "Error"} {
		logs := sink.GetLogs(level, 0, time.Time{})
		if len(logs) != 1 || logs[0].Message != "an error line" {
			t.Errorf("GetLogs(%q) = %v, want the one error line", level, logs)
		}
	}
	for _, level := range []string{"", "ALL", "all"} {
		if logs := sink.GetLogs(level, 0, time.Time{}); len(logs) != 2 {
			t.Errorf("GetLogs(%q) = %v, want both lines", level, logs)
		}
	}
}
