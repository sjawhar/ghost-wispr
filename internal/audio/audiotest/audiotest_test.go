package audiotest_test

import (
	"encoding/json"
	"fmt"
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

// buildSet is one set of build settings a test binary can be built with:
// the race detector (whose race tag selects files too) and build tags.
type buildSet struct {
	race bool
	tags string
}

func (b buildSet) String() string {
	if b.race {
		return fmt.Sprintf("-race -tags %q", b.tags)
	}
	return fmt.Sprintf("-tags %q", b.tags)
}

// buildSets is each set of build settings the check lists the test binaries
// under: with and without the race detector, each with no tags and with the
// kws tag CI tests with (CI runs go test -race -tags kws), and this test
// binary's own.
func buildSets() []buildSet {
	var sets []buildSet
	for _, race := range []bool{false, true} {
		for _, tags := range []string{"", "kws"} {
			sets = append(sets, buildSet{race: race, tags: tags})
		}
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		own := buildSet{}
		for _, s := range info.Settings {
			switch s.Key {
			case "-tags":
				own.tags = s.Value
			case "-race":
				own.race = s.Value == "true"
			}
		}
		if !slices.Contains(sets, own) {
			sets = append(sets, own)
		}
	}
	return sets
}

// uncovered lists, under set, each package of pattern whose test binary
// links internal/audio (linking), and those of them whose test files, as set
// selects them, have no TestMain that calls the guard (missing). o, when not
// nil, is the overlay the go command reads.
func uncovered(t *testing.T, root string, set buildSet, pattern string, o *overlay) (linking, missing []string) {
	t.Helper()
	args := []string{"list", "-e", "-test", "-tags", set.tags}
	if set.race {
		args = append(args, "-race")
	}
	args = append(args, "-f", "{{.ImportPath}}\t{{.Dir}}\t{{join .TestGoFiles \",\"}}\t{{join .XTestGoFiles \",\"}}\t{{join .Deps \",\"}}")
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
// internal/audio itself), under any set of build settings CI tests with: a
// refused open would then not fail the binary. It lists the module's test
// binaries with `go list -test` under each set (buildSets).
func TestEveryTestBinaryThatLinksAudioRefusesRealDevices(t *testing.T) {
	root := moduleRoot(t)
	for _, set := range buildSets() {
		linking, missing := uncovered(t, root, set, "./...", nil)
		// Control: the list holds the test binaries known to link internal/audio.
		for _, want := range []string{audioPkg, module + "/cmd/ghost-wispr", module + "/internal/voice"} {
			if !slices.Contains(linking, want) {
				t.Fatalf("UNKNOWN: under %v, go list names %d test binaries that link internal/audio, and not %s: %v", set, len(linking), want, linking)
			}
		}
		for _, pkg := range missing {
			t.Errorf("%s, under %v: its tests link internal/audio, but no TestMain there calls audiotest.Main, so a refused open would not fail it", pkg, set)
		}
	}
}

// probeFiles is a planted package whose only test file without a TestMain is
// built under constraint, beside a file under its negation that has one.
func probeFiles(dir, name, constraint string) map[string]string {
	return map[string]string{
		dir + "/probe_test.go": "//go:build " + constraint + `

package ` + name + `

import (
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

func TestLinksAudio(t *testing.T) { _ = audio.ErrRealDeviceRefused }
`,
		dir + "/main_test.go": "//go:build !" + constraint + `

package ` + name + `

import (
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/audio/audiotest"
)

func TestMain(m *testing.M) { audiotest.Main(m) }
`,
	}
}

// TestTheCoverageCheckReadsTheTestFilesEachBuildSetSelects fails if the
// check misses a package whose only test file without a TestMain is built
// under the kws tag, or only with the race detector, or counts a TestMain in
// a file the build set excludes. The packages are planted through an
// overlay; nothing is written to the module.
func TestTheCoverageCheckReadsTheTestFilesEachBuildSetSelects(t *testing.T) {
	root := moduleRoot(t)
	for _, tc := range []struct {
		name, constraint string
		without, with    buildSet
	}{
		{"kwsprobe", "kws", buildSet{}, buildSet{tags: "kws"}},
		{"raceprobe", "race", buildSet{}, buildSet{race: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := "internal/audio/audiotest/" + tc.name
			probe := module + "/" + dir
			o := plant(t, root, probeFiles(dir, tc.name, tc.constraint))
			linking, missing := uncovered(t, root, tc.without, "./"+dir+"/", o)
			if !slices.Contains(linking, probe) || slices.Contains(missing, probe) {
				t.Errorf("under %v: linking %v, missing %v; want the planted package linking, with its TestMain", tc.without, linking, missing)
			}
			linking, missing = uncovered(t, root, tc.with, "./"+dir+"/", o)
			if !slices.Contains(linking, probe) || !slices.Contains(missing, probe) {
				t.Errorf("under %v: linking %v, missing %v; want the planted package linking and missing a TestMain, since %s excludes the file that has one", tc.with, linking, missing, tc.constraint)
			}
		})
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
