package tests

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestWarningsLogAtWarnLevel fails if a non-test Go file of the module logs a
// message that begins with "warning" below the WARN level. slog's bridge
// records every line of the log package at INFO, so a unit with
// log_level: warn would drop such a warning.
func TestWarningsLogAtWarnLevel(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate this test")
	}
	repo := filepath.Dir(filepath.Dir(thisFile))
	listed, err := exec.Command("git", "-C", repo, "ls-files", "-z", "--cached", "--others", "--exclude-standard", "--", "*.go").Output()
	if err != nil {
		t.Fatalf("list the module's Go files: %v", err)
	}
	var files []string
	for _, file := range strings.Split(strings.TrimSuffix(string(listed), "\x00"), "\x00") {
		if file != "" && !strings.HasSuffix(file, "_test.go") {
			files = append(files, file)
		}
	}
	// A check that read no file has not passed: it could not see the tree.
	if len(files) == 0 {
		t.Fatalf("git ls-files listed no non-test .go file in %s; the check cannot see the module", repo)
	}
	if !slices.Contains(files, "cmd/ghost-wispr/main.go") {
		t.Fatalf("git ls-files listed %d non-test .go files in %s but not cmd/ghost-wispr/main.go; the check cannot see the module", len(files), repo)
	}
	fset := token.NewFileSet()
	byPackage := map[string][]*ast.File{}
	for _, file := range files {
		parsed, err := parser.ParseFile(fset, filepath.Join(repo, file), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		byPackage[filepath.Dir(file)] = append(byPackage[filepath.Dir(file)], parsed)
	}
	var hits []string
	for _, parsed := range byPackage {
		names := stringNames(parsed)
		for _, file := range parsed {
			for _, hit := range warningsBelowWarn(fset, file, names) {
				rel, err := filepath.Rel(repo, hit.Filename)
				if err != nil {
					rel = hit.Filename
				}
				hits = append(hits, fmt.Sprintf("%s:%d: %s", rel, hit.Line, hit.message))
			}
		}
	}
	slices.Sort(hits)
	if len(hits) > 0 {
		t.Errorf("%d calls in %d non-test .go files log a warning below WARN; log each through a logger's Warn (slog.Warn in package main):\n%s",
			len(hits), len(files), strings.Join(hits, "\n"))
	}
}

// TestWarningsBelowWarnFindsAnInfoWarning runs the check on a snippet, so a
// check that cannot fire fails here instead of passing on the module: each
// form of an INFO warning is found, and no WARN or ERROR call is.
func TestWarningsBelowWarnFindsAnInfoWarning(t *testing.T) {
	const src = `package snippet

const constantWarning = "warning: a constant"

func f() {
	log.Printf("warning: x")
	slog.Warn("x")
	slog.Warn("warning: y")
	logger.WarnContext(ctx, "warning: z")
	slog.Error("warning: e")
	fmt.Errorf("warning: not a log line")
	log.Println(constantWarning)
	log.Print(fmt.Sprintf("warning: %s", "nested"))
	message := "warning: a variable"
	log.Printf("%s", message)
	slog.Warn(constantWarning)
}
`
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, "snippet.go", src, 0)
	if err != nil {
		t.Fatalf("parse snippet: %v", err)
	}
	hits := warningsBelowWarn(fset, parsed, stringNames([]*ast.File{parsed}))
	var lines []int
	for _, hit := range hits {
		lines = append(lines, hit.Line)
	}
	if want := []int{6, 12, 13, 15}; !slices.Equal(lines, want) {
		t.Fatalf("hits = %+v, want the INFO warnings at snippet.go lines %v", hits, want)
	}
}

type warningHit struct {
	token.Position
	message string
}

// warningsBelowWarn returns each call in file whose message begins with
// "warning" (in any case), unless the call logs at WARN or above. names holds
// the string constants and literal-initialized variables of file's package.
func warningsBelowWarn(fset *token.FileSet, file *ast.File, names map[string]string) []warningHit {
	var hits []warningHit
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || buildsString(call) {
			return true
		}
		message, ok := callMessage(call, names)
		if !ok || !strings.HasPrefix(strings.ToLower(message), "warning") || atWarnOrAbove(call) {
			return true
		}
		hits = append(hits, warningHit{Position: fset.Position(call.Pos()), message: message})
		return true
	})
	return hits
}

// stringNames returns the value of each string constant and each variable
// set from a string literal in files, by name. It ignores scope: a name set
// to two values keeps the last one read.
func stringNames(files []*ast.File) map[string]string {
	names := map[string]string{}
	set := func(lhs []*ast.Ident, rhs []ast.Expr) {
		if len(lhs) != len(rhs) {
			return
		}
		for i, name := range lhs {
			if value, ok := literalString(rhs[i]); ok {
				names[name.Name] = value
			}
		}
	}
	for _, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.ValueSpec: // const and var declarations
				set(node.Names, node.Values)
			case *ast.AssignStmt:
				var lhs []*ast.Ident
				for _, expr := range node.Lhs {
					if ident, ok := expr.(*ast.Ident); ok {
						lhs = append(lhs, ident)
					}
				}
				if len(lhs) == len(node.Lhs) {
					set(lhs, node.Rhs)
				}
			}
			return true
		})
	}
	return names
}

// callMessage returns the message of call: its first argument, the argument
// after the context of a slog ...Context method or the writer of fmt.Fprint*,
// or the argument after the context and level of slog's Log and LogAttrs. A
// format that starts with %s or %v starts with its first value, so that value
// is the message.
func callMessage(call *ast.CallExpr, names map[string]string) (string, bool) {
	at := 0
	switch name := calledName(call); {
	case name == "Log" || name == "LogAttrs":
		at = 2
	case strings.HasSuffix(name, "Context") || strings.HasPrefix(name, "Fprint"):
		at = 1
	}
	if len(call.Args) <= at {
		return "", false
	}
	message, ok := leadingString(call.Args[at], names)
	if ok && (strings.HasPrefix(message, "%s") || strings.HasPrefix(message, "%v")) && len(call.Args) > at+1 {
		return leadingString(call.Args[at+1], names)
	}
	return message, ok
}

// buildsString reports whether call builds a string or an error instead of
// logging one: fmt.Sprint*, fmt.Append*, fmt.Errorf and errors.New. The log
// call that writes the result is the one checked.
func buildsString(call *ast.CallExpr) bool {
	fun, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := fun.X.(*ast.Ident)
	if !ok {
		return false
	}
	name := fun.Sel.Name
	switch pkg.Name {
	case "fmt":
		return strings.HasPrefix(name, "Sprint") || strings.HasPrefix(name, "Append") || name == "Errorf"
	case "errors":
		return name == "New"
	}
	return false
}

// atWarnOrAbove reports whether call logs at WARN or above: a Warn or Error
// method or function (slog.Warn, logger.ErrorContext), or Log and LogAttrs
// with slog.LevelWarn or slog.LevelError.
func atWarnOrAbove(call *ast.CallExpr) bool {
	switch calledName(call) {
	case "Warn", "WarnContext", "Error", "ErrorContext":
		return true
	case "Log", "LogAttrs":
		if len(call.Args) < 2 {
			return false
		}
		level, ok := call.Args[1].(*ast.SelectorExpr)
		return ok && (level.Sel.Name == "LevelWarn" || level.Sel.Name == "LevelError")
	}
	return false
}

func calledName(call *ast.CallExpr) string {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return fun.Name
	case *ast.SelectorExpr:
		return fun.Sel.Name
	}
	return ""
}

// leadingString returns the string that starts expr, as far as the source
// shows it: a string literal, a name stringNames resolves, the first part of
// a concatenation ("warning: "+x), or the message fmt.Sprint* builds.
func leadingString(expr ast.Expr, names map[string]string) (string, bool) {
	switch e := expr.(type) {
	case *ast.BasicLit:
		return literalString(e)
	case *ast.Ident:
		value, ok := names[e.Name]
		return value, ok
	case *ast.BinaryExpr:
		if e.Op == token.ADD {
			return leadingString(e.X, names)
		}
	case *ast.ParenExpr:
		return leadingString(e.X, names)
	case *ast.CallExpr:
		if buildsString(e) && strings.HasPrefix(calledName(e), "Sprint") {
			return callMessage(e, names)
		}
	}
	return "", false
}

// literalString is the value of a string literal.
func literalString(expr ast.Expr) (string, bool) {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}
