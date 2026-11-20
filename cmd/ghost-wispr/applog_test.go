package main

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/logging"
	"github.com/sjawhar/ghost-wispr/internal/server"
)

// A line logged after a settings change (on the voice settings page, say) is
// in /api/logs and on stdout, from the log package and from slog's default
// logger alike.
func TestASettingsChangeKeepsTheLogSink(t *testing.T) {
	keepDefaultLogger(t)
	cfgStore := appLogConfigStore(t)
	var stdout bytes.Buffer
	_, sink := startAppLogger(cfgStore, &stdout)
	h, err := server.Handler(fstest.MapFS{"index.html": {Data: []byte("ok")}}, server.NewHub(), nil, &server.ControlHooks{GetLogs: sink.GetLogs}, "", nil, cfgStore)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	slog.Info("logged before a settings change") // shows the check can read a line at all

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPatch, "/api/config", strings.NewReader(`{"voice":{"engine":"gptlive"}}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("PATCH /api/config: %d %s", rr.Code, rr.Body.String())
	}
	log.Printf("logged by the log package after a settings change")
	slog.Info("logged by slog after a settings change")

	logged := apiLogMessages(t, h)
	for _, want := range []string{"logged before a settings change", "logged by the log package after a settings change", "logged by slog after a settings change"} {
		if !logged[want] {
			t.Errorf("/api/logs does not hold %q; it holds %v", want, logged)
		}
		if !strings.Contains(stdout.String(), `"message":"`+want+`"`) {
			t.Errorf("stdout does not hold %q; it holds:\n%s", want, stdout.String())
		}
	}
}

// No settings-change callback written in place (a function literal given to
// onConfigChange or a config store's OnChange) builds a logger or sets slog's
// default: startAppLogger builds the app's only logger, and a callback that
// replaced it would leave the lines logged after a change out of /api/logs.
func TestNoSettingsChangeCallbackReplacesTheLogger(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	callbacks := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || !isSettingsChangeRegistration(call.Fun) {
				return true
			}
			for _, arg := range call.Args {
				callback, ok := arg.(*ast.FuncLit)
				if !ok {
					continue
				}
				callbacks++
				ast.Inspect(callback.Body, func(m ast.Node) bool {
					if inner, ok := m.(*ast.CallExpr); ok {
						if called := qualifiedName(inner.Fun); called == "slog.SetDefault" || called == "logging.SetDefault" || called == "logging.New" {
							t.Errorf("%s: a settings-change callback calls %s", fset.Position(inner.Pos()), called)
						}
					}
					return true
				})
			}
			return true
		})
	}
	if callbacks == 0 {
		t.Fatal("found no settings-change callback written in place, so the check read none")
	}
}

func isSettingsChangeRegistration(fun ast.Expr) bool {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name == "onConfigChange"
	case *ast.SelectorExpr:
		return f.Sel.Name == "OnChange"
	}
	return false
}

// qualifiedName is "pkg.Func" for a call of a package's function, else "".
func qualifiedName(fun ast.Expr) string {
	selector, ok := fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	pkg, ok := selector.X.(*ast.Ident)
	if !ok {
		return ""
	}
	return pkg.Name + "." + selector.Sel.Name
}

// A settings change of the log level applies to a logger the app handed out
// before it, and to slog's default logger.
func TestASettingsChangeSetsTheLogLevelOfEveryLogger(t *testing.T) {
	keepDefaultLogger(t)
	cfgStore := appLogConfigStore(t)
	logger, sink := startAppLogger(cfgStore, io.Discard)
	component := logging.WithModule(logger, "session") // as session.NewManager takes it at start

	component.Info("info line before the change") // shows the check can read a line at all
	component.Debug("debug line before the change")
	if err := cfgStore.Update(func(c *config.Config) { c.LogLevel = "debug" }); err != nil {
		t.Fatal(err)
	}
	component.Debug("debug line after the change")
	slog.Debug("default debug line after the change")

	logged := map[string]bool{}
	for _, entry := range sink.GetLogs("", 0, time.Time{}) {
		logged[entry.Message] = true
	}
	if logged["debug line before the change"] {
		t.Errorf("a debug line was logged at level info")
	}
	for _, want := range []string{"info line before the change", "debug line after the change", "default debug line after the change"} {
		if !logged[want] {
			t.Errorf("the log does not hold %q after the level changed to debug; it holds %v", want, logged)
		}
	}
}

// The sink gets each line even when stdout fails, or takes only part of it.
func TestAppLoggerKeepsTheSinkWhenStdoutFails(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write appLogWriteFunc
	}{
		{"write error", func([]byte) (int, error) { return 0, io.ErrClosedPipe }},
		{"short write", func(p []byte) (int, error) { return len(p) - 1, nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keepDefaultLogger(t)
			logger, sink := startAppLogger(appLogConfigStore(t), tc.write)
			logger.Info("component record with broken stdout")
			log.Print("standard record with broken stdout")
			slog.Info("default record with broken stdout")

			logged := map[string]bool{}
			for _, entry := range sink.GetLogs("", 0, time.Time{}) {
				logged[entry.Message] = true
			}
			for _, want := range []string{"component record with broken stdout", "standard record with broken stdout", "default record with broken stdout"} {
				if !logged[want] {
					t.Errorf("the sink lost %q when stdout failed; it holds %v", want, logged)
				}
			}
		})
	}
}

type appLogWriteFunc func([]byte) (int, error)

func (write appLogWriteFunc) Write(p []byte) (int, error) { return write(p) }

// keepDefaultLogger puts slog's default logger, and the log package's output,
// back as they were when the test ends.
func keepDefaultLogger(t *testing.T) {
	t.Helper()
	logger, writer, flags := slog.Default(), log.Writer(), log.Flags()
	t.Cleanup(func() {
		slog.SetDefault(logger)
		log.SetOutput(writer)
		log.SetFlags(flags)
	})
}

func appLogConfigStore(t *testing.T) *config.Store {
	t.Helper()
	t.Setenv(config.EnvPrefix+"LOG_LEVEL", "info")
	cfgStore, _, err := config.NewStore("")
	if err != nil {
		t.Fatalf("config store: %v", err)
	}
	return cfgStore
}

func apiLogMessages(t *testing.T, h http.Handler) map[string]bool {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/logs?limit=1000", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/logs: %d %s", rr.Code, rr.Body.String())
	}
	var entries []logging.LogEntry
	if err := json.Unmarshal(rr.Body.Bytes(), &entries); err != nil {
		t.Fatalf("decode /api/logs: %v", err)
	}
	messages := map[string]bool{}
	for _, entry := range entries {
		messages[entry.Message] = true
	}
	return messages
}
