package audiotest_test

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
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

// moduleRoot is the directory of the module's go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	gomod, err := exec.Command(goTool(t), "env", "GOMOD").Output()
	if err != nil || strings.TrimSpace(string(gomod)) == "" {
		t.Fatalf("UNKNOWN: go env GOMOD: %q, %v", gomod, err)
	}
	return filepath.Dir(strings.TrimSpace(string(gomod)))
}

// overlay is a go command overlay (-overlay): files the go command sees in
// the module that are not on disk, for a planted package.
type overlay struct {
	path    string            // its JSON file
	replace map[string]string // each file in the module, to the file it reads
}

// plant writes files, by their paths in the module, as an overlay.
func plant(t *testing.T, root string, files map[string]string) *overlay {
	t.Helper()
	dir := t.TempDir()
	o := &overlay{path: filepath.Join(dir, "overlay.json"), replace: map[string]string{}}
	for rel, src := range files {
		real := filepath.Join(dir, strings.ReplaceAll(rel, "/", "_"))
		if err := os.WriteFile(real, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
		o.replace[filepath.Join(root, filepath.FromSlash(rel))] = real
	}
	data, err := json.Marshal(map[string]any{"Replace": o.replace})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(o.path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return o
}

// buildTagSets is each set of build tags the check lists the test binaries
// under: none, the kws tag CI tests with, and this test binary's own tags.
func buildTagSets() []string {
	sets := []string{"", "kws"}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "-tags" && !slices.Contains(sets, s.Value) {
				sets = append(sets, s.Value)
			}
		}
	}
	return sets
}

// uncovered lists, under tags, each package of pattern whose test binary
// links internal/audio (linking), and those of them whose test files, as tags
// select them, have no TestMain that calls the guard (missing). o, when not
// nil, is the overlay the go command reads.
func uncovered(t *testing.T, root, tags, pattern string, o *overlay) (linking, missing []string) {
	t.Helper()
	args := []string{"list", "-e", "-test", "-tags", tags, "-f",
		"{{.ImportPath}}\t{{.Dir}}\t{{join .TestGoFiles \",\"}}\t{{join .XTestGoFiles \",\"}}\t{{join .Deps \",\"}}"}
	if o != nil {
		args = append(args, "-overlay", o.path)
	}
	list := exec.Command(goTool(t), append(args, pattern)...)
	list.Dir = root
	out, err := list.Output()
	if err != nil {
		t.Fatalf("UNKNOWN: go %s: %v", strings.Join(args, " "), err)
	}
	testFiles := map[string][]string{}
	for line := range strings.Lines(string(out)) {
		fields := strings.Split(strings.TrimRight(line, "\n"), "\t")
		if len(fields) != 5 {
			t.Fatalf("UNKNOWN: go list printed %q", line)
		}
		path, dir, deps := fields[0], fields[1], strings.Split(fields[4], ",")
		if !strings.Contains(path, " ") && !strings.HasSuffix(path, ".test") {
			for _, name := range slices.Concat(strings.Split(fields[2], ","), strings.Split(fields[3], ",")) {
				if name != "" {
					testFiles[path] = append(testFiles[path], filepath.Join(dir, name))
				}
			}
		}
		pkg, isTest := strings.CutSuffix(path, ".test")
		if isTest && slices.ContainsFunc(deps, func(d string) bool { return d == audioPkg || strings.HasPrefix(d, audioPkg+" [") }) {
			linking = append(linking, pkg)
		}
	}
	for _, pkg := range linking {
		if !refusesRealDevices(t, testFiles[pkg], o) {
			missing = append(missing, pkg)
		}
	}
	return linking, missing
}

// TestEveryTestBinaryThatLinksAudioRefusesRealDevices fails if the tests of a
// package that links internal/audio, directly or through another package, run
// without audiotest.Main in their TestMain (audio.RunWithoutRealDevices in
// internal/audio itself), under any set of build tags CI tests with: a refused
// open would then not fail the binary. It lists the module's test binaries
// with `go list -test` under each set (buildTagSets).
func TestEveryTestBinaryThatLinksAudioRefusesRealDevices(t *testing.T) {
	root := moduleRoot(t)
	for _, tags := range buildTagSets() {
		linking, missing := uncovered(t, root, tags, "./...", nil)
		// Control: the list holds the test binaries known to link internal/audio.
		for _, want := range []string{audioPkg, module + "/cmd/ghost-wispr", module + "/internal/voice"} {
			if !slices.Contains(linking, want) {
				t.Fatalf("UNKNOWN: under tags %q, go list names %d test binaries that link internal/audio, and not %s: %v", tags, len(linking), want, linking)
			}
		}
		for _, pkg := range missing {
			t.Errorf("%s, under tags %q: its tests link internal/audio, but no TestMain there calls audiotest.Main, so a refused open would not fail it", pkg, tags)
		}
	}
}

// TestTheCoverageCheckReadsTheTestFilesEachTagSetSelects fails if the check
// misses a package whose only test file without a TestMain is built under the
// kws tag, or counts a TestMain in a file the tags exclude. The package is
// planted through an overlay; nothing is written to the module.
func TestTheCoverageCheckReadsTheTestFilesEachTagSetSelects(t *testing.T) {
	root := moduleRoot(t)
	const probe = module + "/internal/audio/audiotest/kwsprobe"
	o := plant(t, root, map[string]string{
		"internal/audio/audiotest/kwsprobe/probe_test.go": `//go:build kws

package kwsprobe

import (
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

func TestLinksAudio(t *testing.T) { _ = audio.ErrRealDeviceRefused }
`,
		"internal/audio/audiotest/kwsprobe/main_test.go": `//go:build !kws

package kwsprobe

import (
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/audio/audiotest"
)

func TestMain(m *testing.M) { audiotest.Main(m) }
`,
	})
	linking, missing := uncovered(t, root, "", "./internal/audio/audiotest/kwsprobe/", o)
	if !slices.Contains(linking, probe) || slices.Contains(missing, probe) {
		t.Errorf("without tags: linking %v, missing %v; want the planted package linking, with its TestMain", linking, missing)
	}
	linking, missing = uncovered(t, root, "kws", "./internal/audio/audiotest/kwsprobe/", o)
	if !slices.Contains(linking, probe) || !slices.Contains(missing, probe) {
		t.Errorf("under kws: linking %v, missing %v; want the planted package linking and missing a TestMain, since kws excludes the file that has one", linking, missing)
	}
}

// TestAnOpenAsAPackageInitializesFailsTheBinary fails if a test binary
// passes, or an open is not refused, when a package variable's initializer in
// it opens a capture: Go runs it before TestMain. The package is planted
// through an overlay, and the PCM it opens is one ALSA does not define, so a
// broken guard still opens no device.
func TestAnOpenAsAPackageInitializesFailsTheBinary(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the planted package opens an ALSA capture, which only Linux has")
	}
	root := moduleRoot(t)
	o := plant(t, root, map[string]string{
		"internal/audio/audiotest/initprobe/probe_test.go": `package initprobe

import (
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/audio/audiotest"
)

// openedAtInit opens a capture as the package initializes, before TestMain.
var openedAtInit = func() error {
	c, err := audio.OpenALSACapture("gw-no-such-pcm", 16000)
	if err == nil {
		_ = c.Close()
	}
	return err
}()

func TestMain(m *testing.M) { audiotest.Main(m) }

func TestReportsTheOpen(t *testing.T) { t.Logf("the open at init answered: %v", openedAtInit) }
`,
	})
	// The planted directory exists only in the overlay, where go test can
	// neither vet it nor run its binary, so the binary is built (-c) and run.
	binary := filepath.Join(t.TempDir(), "initprobe.test")
	build := exec.Command(goTool(t), "test", "-c", "-vet=off", "-o", binary, "-overlay", o.path, "./internal/audio/audiotest/initprobe/")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("UNKNOWN: the planted test binary did not build: %v\n%s", err, out)
	}
	out, err := exec.Command(binary, "-test.count=1", "-test.v").CombinedOutput()
	if err == nil {
		t.Errorf("the planted test binary passed:\n%s", out)
	}
	for _, want := range []string{
		"--- PASS: TestReportsTheOpen", // control: the binary built, and its test ran
		"the open at init answered: audio: a test may not open a real audio device: ALSA capture gw-no-such-pcm",
		"audio: a test reached a real audio device: ALSA capture gw-no-such-pcm",
		"FAIL: the tests tried to open a real audio device 1 times",
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("the planted test binary's output lacks %q:\n%s", want, out)
		}
	}
}

// refusesRealDevices reports whether a TestMain in files calls
// audiotest.Main, or audio.RunWithoutRealDevices. A file o plants is read
// from where o has it.
func refusesRealDevices(t *testing.T, files []string, o *overlay) bool {
	t.Helper()
	fset := token.NewFileSet()
	for _, name := range files {
		read := name
		if o != nil && o.replace[name] != "" {
			read = o.replace[name]
		}
		src, err := os.ReadFile(read)
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
