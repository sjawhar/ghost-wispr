package main

import (
	"io"
	"log/slog"
)

// LevelNotice is Cloud Logging's NOTICE, between INFO and WARNING. Only the
// watcher's WATCHER-PASS line uses it, so an alert can count that line alone.
const LevelNotice = slog.LevelInfo + 2

// componentKey is the field that marks a line as ours. The call pool's log
// exclusion drops every line without it.
const componentKey = "ghost_wispr_component"

// newLogger writes one JSON object per line: severity (DEBUG, INFO, NOTICE,
// WARNING, ERROR), message, ghost_wispr_component, and the record's
// attributes. Cloud Logging reads severity and message from those keys.
func newLogger(w io.Writer, component string, level slog.Leveler) *slog.Logger {
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) > 0 {
				return a
			}
			switch a.Key {
			case slog.LevelKey:
				lvl, _ := a.Value.Any().(slog.Level)
				return slog.String("severity", severity(lvl))
			case slog.MessageKey:
				a.Key = "message"
			}
			return a
		},
	})
	return slog.New(h.WithAttrs([]slog.Attr{slog.String(componentKey, component)}))
}

// severity names a slog level as Cloud Logging does.
func severity(l slog.Level) string {
	switch {
	case l < slog.LevelInfo:
		return "DEBUG"
	case l < LevelNotice:
		return "INFO"
	case l < slog.LevelWarn:
		return "NOTICE"
	case l < slog.LevelError:
		return "WARNING"
	default:
		return "ERROR"
	}
}
