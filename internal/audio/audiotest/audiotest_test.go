package audiotest_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/audio/audiotest"
)

func TestMain(m *testing.M) { audiotest.Main(m) }

const (
	module   = "github.com/sjawhar/ghost-wispr"
	audioPkg = module + "/internal/audio"
)

// goTool is the go command on PATH, which `go test` runs with.
func goTool(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("UNKNOWN: no go command on PATH, so the test binaries cannot be listed: %v", err)
	}
	return path
}

// TestEveryTestBinaryThatLinksAudioRefusesRealDevices fails if the tests of a
// package that links internal/audio, directly or through another package, run
// without audiotest.Main in their TestMain (audio.RunWithoutRealDevices in
// internal/audio itself): those tests could open a real microphone or
// speaker. It lists the module's test binaries with `go list -test`.
func TestEveryTestBinaryThatLinksAudioRefusesRealDevices(t *testing.T) {
	gomod, err := exec.Command(goTool(t), "env", "GOMOD").Output()
	if err != nil {
		t.Fatalf("UNKNOWN: go env GOMOD: %v", err)
	}
	list := exec.Command(goTool(t), "list", "-e", "-test", "-f", "{{.ImportPath}}\t{{.Dir}}\t{{join .Deps \",\"}}", "./...")
	list.Dir = filepath.Dir(strings.TrimSpace(string(gomod)))
	out, err := list.Output()
	if err != nil {
		t.Fatalf("UNKNOWN: go list -test ./...: %v", err)
	}
	dirs := map[string]string{}
	var linking []string
	for line := range strings.Lines(string(out)) {
		path, rest, _ := strings.Cut(strings.TrimSpace(line), "\t")
		dir, deps, _ := strings.Cut(rest, "\t")
		if !strings.Contains(path, " ") && !strings.HasSuffix(path, ".test") {
			dirs[path] = dir
		}
		pkg, isTest := strings.CutSuffix(path, ".test")
		if isTest && slices.ContainsFunc(strings.Split(deps, ","), func(d string) bool { return d == audioPkg || strings.HasPrefix(d, audioPkg+" [") }) {
			linking = append(linking, pkg)
		}
	}
	// Control: the list holds the test binaries known to link internal/audio.
	for _, want := range []string{audioPkg, module + "/cmd/ghost-wispr", module + "/internal/voice"} {
		if !slices.Contains(linking, want) {
			t.Fatalf("UNKNOWN: go list names %d test binaries that link internal/audio, and not %s: %v", len(linking), want, linking)
		}
	}
	for _, pkg := range linking {
		if !refusesRealDevices(t, dirs[pkg]) {
			t.Errorf("%s: its tests link internal/audio, but no TestMain there calls audiotest.Main, so a test of it could open a real microphone or speaker", pkg)
		}
	}
}

// refusesRealDevices reports whether a TestMain in dir's test files calls
// audiotest.Main, or audio.RunWithoutRealDevices.
func refusesRealDevices(t *testing.T, dir string) bool {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*_test.go"))
	if err != nil || dir == "" {
		t.Fatalf("UNKNOWN: the test files of %q: %v", dir, err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Name.Name != "TestMain" || fn.Body == nil {
				continue
			}
			found := false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch call := n.(type) {
				case *ast.SelectorExpr:
					if x, ok := call.X.(*ast.Ident); ok && x.Name == "audiotest" && call.Sel.Name == "Main" {
						found = true
					}
				case *ast.Ident:
					if call.Name == "RunWithoutRealDevices" {
						found = true
					}
				}
				return !found
			})
			if found {
				return true
			}
		}
	}
	return false
}
