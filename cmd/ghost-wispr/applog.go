package main

import (
	"io"
	"log/slog"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/logging"
)

// logSinkLines is how many of the last log lines /api/logs can serve.
const logSinkLines = 1000

// startAppLogger builds the app's logger and makes it slog's default, and so
// the log package's: JSON lines to stdout and to the sink that /api/logs
// reads, at the level the config names. It is the logger's one constructor: a
// settings change sets the level of the logger every part of the app already
// holds, and nothing builds another, so no settings change can leave a logger
// without the sink.
func startAppLogger(cfgStore *config.Store, stdout io.Writer) (*slog.Logger, *logging.LogSink) {
	sink := logging.NewLogSink(logSinkLines)
	level := new(slog.LevelVar)
	level.Set(logging.ParseLevel(cfgStore.Get().LogLevel))
	logger := logging.New(io.MultiWriter(sink, stdout), level)
	logging.SetDefault(logger)
	onConfigChange(cfgStore, func(cfg *config.Config) {
		level.Set(logging.ParseLevel(cfg.LogLevel))
	})
	return logger, sink
}
