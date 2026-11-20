package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// mainCalls returns, for each function or method name in names, the
// positions in main() of the calls to it, and of the go statements that
// start it.
func mainCalls(t *testing.T, names ...string) (calls, goStmts map[string][]token.Pos, fset *token.FileSet) {
	t.Helper()
	fset = token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var mainFunc *ast.FuncDecl
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == "main" {
			mainFunc = fn
		}
	}
	if mainFunc == nil {
		t.Fatal("main.go has no func main")
	}
	want := map[string]bool{}
	for _, name := range names {
		want[name] = true
	}
	called := func(call *ast.CallExpr) string {
		switch fun := call.Fun.(type) {
		case *ast.Ident:
			return fun.Name
		case *ast.SelectorExpr:
			return fun.Sel.Name
		}
		return ""
	}
	calls, goStmts = map[string][]token.Pos{}, map[string][]token.Pos{}
	ast.Inspect(mainFunc.Body, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.GoStmt:
			if name := called(node.Call); want[name] {
				goStmts[name] = append(goStmts[name], node.Pos())
			}
		case *ast.CallExpr:
			if name := called(node); want[name] {
				calls[name] = append(calls[name], node.Pos())
			}
		}
		return true
	})
	return calls, goStmts, fset
}

// TestMainWiresTheStopsRecovery fails if main() stops making the calls the
// stop and start tests drive (startAgain, endSessionAtShutdown): the
// recovery of the refinements a stop cut off, then, after the recording
// directories are cleaned up, their retries in the background; and the stop's
// end of the session within the shutdown deadline. Those tests run the calls
// themselves; this pins main() to them, so dropping one from main fails here.
func TestMainWiresTheStopsRecovery(t *testing.T) {
	calls, goStmts, fset := mainCalls(t, "RecoverStaleRefinements", "cleanupRecordingDirectories", "resumeStoppedRefinements", "endSessionAtShutdown", "WaitForLifecyclePublishes")
	one := func(positions map[string][]token.Pos, name, what string) token.Pos {
		t.Helper()
		if len(positions[name]) != 1 {
			t.Fatalf("main() %s %s %d times, want once", what, name, len(positions[name]))
		}
		return positions[name][0]
	}
	recovery := one(calls, "RecoverStaleRefinements", "calls")
	cleanup := one(calls, "cleanupRecordingDirectories", "calls")
	retries := one(goStmts, "resumeStoppedRefinements", "starts in a go statement")
	if !(recovery < cleanup && cleanup < retries) {
		t.Fatalf("main() recovers at %v, cleans up at %v and starts the retries at %v: want the recovery, then the cleanup, then the retries",
			fset.Position(recovery), fset.Position(cleanup), fset.Position(retries))
	}
	one(calls, "endSessionAtShutdown", "calls")
	if len(calls["WaitForLifecyclePublishes"]) != 0 {
		t.Fatalf("main() waits for the lifecycle events itself at %v, outside endSessionAtShutdown and its shutdown deadline", fset.Position(calls["WaitForLifecyclePublishes"][0]))
	}
}
