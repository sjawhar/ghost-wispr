package logging

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

const RequestIDHeader = "X-Request-ID"

type contextKey string

const loggerContextKey contextKey = "ghost_wispr_logger"

func New(output io.Writer, level string) *slog.Logger {
	handlerOptions := &slog.HandlerOptions{
		Level: parseLevel(level),
		ReplaceAttr: func(_ []string, attr slog.Attr) slog.Attr {
			switch attr.Key {
			case slog.TimeKey:
				attr.Key = "timestamp"
			case slog.MessageKey:
				attr.Key = "message"
			}
			return attr
		},
	}

	return slog.New(slog.NewJSONHandler(output, handlerOptions))
}

func WithModule(logger *slog.Logger, module string) *slog.Logger {
	if logger == nil {
		logger = slog.Default()
	}

	if strings.TrimSpace(module) == "" {
		return logger
	}

	return logger.With("module", module)
}

func ContextWithLogger(ctx context.Context, logger *slog.Logger) context.Context {
	if logger == nil {
		logger = slog.Default()
	}
	return context.WithValue(ctx, loggerContextKey, logger)
}

func FromContext(ctx context.Context, fallback *slog.Logger) *slog.Logger {
	if logger, ok := ctx.Value(loggerContextKey).(*slog.Logger); ok && logger != nil {
		return logger
	}
	if fallback != nil {
		return fallback
	}
	return slog.Default()
}

func RequestIDMiddleware(base *slog.Logger, generateID func() string) func(http.Handler) http.Handler {
	if base == nil {
		base = slog.Default()
	}
	if generateID == nil {
		generateID = newRequestID
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestID := generateID()
			w.Header().Set(RequestIDHeader, requestID)

			logger := base.With("request_id", requestID)
			ctx := ContextWithLogger(r.Context(), logger)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func parseLevel(level string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func newRequestID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "request-id-unavailable"
	}
	return hex.EncodeToString(b)
}
