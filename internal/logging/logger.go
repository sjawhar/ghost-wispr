package logging

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
)

const RequestIDHeader = "X-Request-ID"

type contextKey string

const loggerContextKey contextKey = "ghost_wispr_logger"

const moduleKey = "module"

// New returns a JSON logger that writes to output at level: a slog.Level, or
// a *slog.LevelVar whose level can change while the logger runs. An error
// attribute that holds a provider's error (one with LogAttrs, as
// providererr.Error has) is written as its text and those fields, and a line
// a provider's SDK logs is withheld (sdkHandler).
func New(output io.Writer, level slog.Leveler) *slog.Logger {
	handlerOptions := &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(_ []string, attr slog.Attr) slog.Attr {
			switch attr.Key {
			case slog.TimeKey:
				attr.Key = "timestamp"
			case slog.MessageKey:
				attr.Key = "message"
			default:
				if attr.Value.Kind() == slog.KindAny {
					if err, ok := attr.Value.Any().(error); ok {
						if fields, ok := providerFields(err); ok {
							attr.Value = fields
						}
					}
				}
			}
			return attr
		},
	}

	return slog.New(sdkHandler{slog.NewJSONHandler(output, handlerOptions)})
}

// SetDefault makes logger slog's default, and so the log package's. It first
// sets the log package's flags to name each line's caller, so slog's bridge
// records the function that wrote a line (it clears the flags itself), and a
// line a provider's SDK writes with the log package is withheld too
// (sdkHandler).
func SetDefault(logger *slog.Logger) {
	log.SetFlags(log.Lshortfile)
	slog.SetDefault(logger)
}

// googleSDKLogging is the variable that turns on the logging of Google's
// SDKs (Drive, Google auth).
const googleSDKLogging = "GOOGLE_SDK_GO_LOGGING_LEVEL"

// WithholdGoogleSDKLogging turns off the logging GOOGLE_SDK_GO_LOGGING_LEVEL
// turns on in Google's SDKs, and reports whether it was on. That logger
// writes straight to stderr, the journal, around the app's logger, and at
// debug it writes each request whole: a folder's name, which holds a
// session's title, or a document's content. Call it before any Google client
// is built.
func WithholdGoogleSDKLogging() bool {
	if os.Getenv(googleSDKLogging) == "" {
		return false
	}
	_ = os.Unsetenv(googleSDKLogging)
	return true
}

// logFielder is an error that names its fields for a log record, as
// providererr.Error does.
type logFielder interface{ LogAttrs() []slog.Attr }

// providerFields is err written as its text and the fields of the provider's
// error it holds, if it holds one.
func providerFields(err error) (slog.Value, bool) {
	var fielder logFielder
	if !errors.As(err, &fielder) {
		return slog.Value{}, false
	}
	return slog.GroupValue(append([]slog.Attr{slog.String("text", err.Error())}, fielder.LogAttrs()...)...), true
}

// providerSDKs are the module paths of the providers' SDKs. A line one of
// them logs can quote what the provider was sent or answered (genai logs a
// stream's read error, and a response it could not map), so its text is
// withheld.
var providerSDKs = []string{
	"google.golang.org/genai",
	"google.golang.org/api",
	"github.com/sashabaranov/go-openai",
	"github.com/anthropics/anthropic-sdk-go",
	"github.com/deepgram/deepgram-go-sdk",
}

// SDKLineWithheld is the message of a record a provider's SDK logged.
const SDKLineWithheld = "a provider's SDK logged a line; its text is withheld: it can quote what the provider was sent or answered"

// sdkHandler writes a record that a provider's SDK logged, found by the
// function that logged it, as SDKLineWithheld and the SDK's path alone.
type sdkHandler struct{ next slog.Handler }

func (h sdkHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

//nolint:gocritic // slog.Handler's Handle takes the record by value.
func (h sdkHandler) Handle(ctx context.Context, record slog.Record) error {
	if sdk := providerSDK(record.PC); sdk != "" {
		withheld := slog.NewRecord(record.Time, record.Level, SDKLineWithheld, record.PC)
		withheld.AddAttrs(slog.String("sdk", sdk))
		record = withheld
	}
	return h.next.Handle(ctx, record)
}

func (h sdkHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return sdkHandler{h.next.WithAttrs(attrs)}
}

func (h sdkHandler) WithGroup(name string) slog.Handler {
	return sdkHandler{h.next.WithGroup(name)}
}

// sdkOfPC caches providerSDK by program counter: a program logs from few
// places.
var sdkOfPC sync.Map

// providerSDK is the path of the provider SDK whose function is at pc, or "".
func providerSDK(pc uintptr) string {
	if pc == 0 {
		return ""
	}
	if sdk, ok := sdkOfPC.Load(pc); ok {
		return sdk.(string)
	}
	frame, _ := runtime.CallersFrames([]uintptr{pc}).Next()
	sdk := ""
	for _, path := range providerSDKs {
		if rest, ok := strings.CutPrefix(frame.Function, path); ok && rest != "" && (rest[0] == '.' || rest[0] == '/') {
			sdk = path
			break
		}
	}
	sdkOfPC.Store(pc, sdk)
	return sdk
}

// WithModule returns logger with each record naming module in its "module"
// attribute, after the attributes the logger binds. On a logger that names a
// module already, it names module in that one's place, so a record holds one
// "module" key: with two, a reader keeps whichever its JSON library picks. A
// module named after a group opened is in that group.
func WithModule(logger *slog.Logger, module string) *slog.Logger {
	if logger == nil {
		logger = slog.Default()
	}

	if strings.TrimSpace(module) == "" {
		return logger
	}

	bare := logger.Handler()
	if named, ok := bare.(*moduleHandler); ok {
		bare = named.bare
	}
	return slog.New(newModuleHandler(bare, module))
}

// moduleHandler writes records through named: bare, which holds every
// attribute but the module, with the module bound last. Each attribute is
// bound once, on bare, and named is bare with the module bound again, so a
// value is rendered once, as it was when it was bound, and naming another
// module binds that one on bare.
//
// Once a group opens, the module stays outside it: named then binds each
// later attribute itself, as bare does. Such an attribute is rendered once
// before either binds it (renderAttrs), so both hold the same text.
type moduleHandler struct {
	bare    slog.Handler
	module  string
	named   slog.Handler
	grouped bool // a group opened since the module was bound on named
}

func newModuleHandler(bare slog.Handler, module string) *moduleHandler {
	return &moduleHandler{bare: bare, module: module, named: bare.WithAttrs([]slog.Attr{slog.String(moduleKey, module)})}
}

func (h *moduleHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.named.Enabled(ctx, level)
}

//nolint:gocritic // slog.Handler's Handle takes the record by value.
func (h *moduleHandler) Handle(ctx context.Context, record slog.Record) error {
	return h.named.Handle(ctx, record)
}

func (h *moduleHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	if !h.grouped {
		return newModuleHandler(h.bare.WithAttrs(attrs), h.module)
	}
	renderAttrs(attrs)
	return &moduleHandler{bare: h.bare.WithAttrs(slices.Clone(attrs)), module: h.module, named: h.named.WithAttrs(attrs), grouped: true}
}

func (h *moduleHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return &moduleHandler{bare: h.bare.WithGroup(name), module: h.module, named: h.named.WithGroup(name), grouped: true}
}

// renderAttrs renders each value of attrs, in groups too, in place, as
// slog's JSON handler renders it: a LogValuer is resolved, an error that is no
// json.Marshaler becomes its text, and any other value of kind Any becomes its
// JSON, so each runs once however many handlers bind the result. A value whose
// JSON fails stays, and each handler writes the same failure.
func renderAttrs(attrs []slog.Attr) {
	for i := range attrs {
		value := attrs[i].Value.Resolve()
		switch value.Kind() {
		case slog.KindGroup:
			group := slices.Clone(value.Group())
			renderAttrs(group)
			value = slog.GroupValue(group...)
		case slog.KindAny:
			value = renderAny(value)
		}
		attrs[i].Value = value
	}
}

func renderAny(value slog.Value) slog.Value {
	v := value.Any()
	if err, ok := v.(error); ok {
		if _, marshals := v.(json.Marshaler); !marshals {
			return slog.StringValue(err.Error())
		}
	}
	var rendered bytes.Buffer
	encoder := json.NewEncoder(&rendered)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(v); err != nil {
		return value
	}
	return slog.AnyValue(json.RawMessage(bytes.TrimSuffix(rendered.Bytes(), []byte("\n"))))
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

// ParseLevel is the level a config names: "debug", "info", "warn" (or
// "warning") or "error", in any case. Any other name is info.
func ParseLevel(level string) slog.Level {
	parsed, _ := LevelNamed(level)
	return parsed
}

// LevelNamed is the level a config names, "debug", "info", "warn" (or
// "warning") or "error" in any case, and whether level is one of them. Any
// other name is info, the level the app logs at for it.
func LevelNamed(level string) (slog.Level, bool) {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return slog.LevelDebug, true
	case "info":
		return slog.LevelInfo, true
	case "warn", "warning":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	default:
		return slog.LevelInfo, false
	}
}

func newRequestID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "request-id-unavailable"
	}
	return hex.EncodeToString(b)
}
