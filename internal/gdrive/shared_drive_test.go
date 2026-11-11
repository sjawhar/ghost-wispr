package gdrive

import (
	"context"
	"fmt"
	"go/ast"
	"go/types"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestEveryDriveCallSetsSupportsAllDrives keeps every Drive API request that
// supports shared drives configured for the shared drive the application uses.
func TestEveryDriveCallSetsSupportsAllDrives(t *testing.T) {
	for _, finding := range driveSharedParameterFailures(t, moduleRoot(t), nil) {
		t.Error(finding)
	}
}

func TestUploadIntoASharedDriveRoot(t *testing.T) {
	wire, syncer := newWireDrive(t)
	const sharedDriveID = "shared-drive-root"
	wire.rootID = sharedDriveID
	syncer.folderID = sharedDriveID

	err := syncer.UploadInto(context.Background(), sharedDriveID, []SyncFile{{
		Name:        "transcript.md",
		MimeType:    "application/vnd.google-apps.document",
		Content:     []byte("shared-drive transcript"),
		ContentType: "text/plain",
	}})
	if err != nil {
		t.Fatalf("upload into shared-drive root: %v", err)
	}
	if got := wire.in(sharedDriveID)["transcript.md"]; len(got) != 1 {
		t.Fatalf("files in shared-drive root: %d transcript.md files, want 1", len(got))
	}
}

const sharedDriveParameterProbe = `package gdrive

func (s *Syncer) probeGetWithoutSupportsAllDrives(id string) error {
	_, err := s.service.Files.Get(id).Do() // want
	return err
}

func (s *Syncer) probeGetWithSupportsAllDrivesFalse(id string) error {
	_, err := s.service.Files.Get(id).SupportsAllDrives(false).Do() // want
	return err
}

func (s *Syncer) probeListWithoutIncludeItemsFromAllDrives() error {
	_, err := s.service.Files.List().SupportsAllDrives(true).Do() // want
	return err
}

func (s *Syncer) probeListWithIncludeItemsFromAllDrivesFalse() error {
	_, err := s.service.Files.List().SupportsAllDrives(true).IncludeItemsFromAllDrives(false).Do() // want
	return err
}

func (s *Syncer) probeSharedDriveParametersSet() error {
	_, err := s.service.Files.List().SupportsAllDrives(true).IncludeItemsFromAllDrives(true).Do()
	return err
}

func (s *Syncer) probeExport(id string) error {
	response, err := s.service.Files.Export(id, "text/plain").Download()
	if response != nil {
		_ = response.Body.Close()
	}
	return err
}
`

// TestTheSharedDriveParameterCheckFindsEveryForm is a planted-probe control:
// it proves that the static guard reports each missing or false shared-drive
// parameter while leaving a valid list and Files.Export (which has no
// SupportsAllDrives option) alone.
func TestTheSharedDriveParameterCheckFindsEveryForm(t *testing.T) {
	root := moduleRoot(t)
	path := filepath.Join(root, "internal", "gdrive", "zz_shared_drive_probe.go")
	want := map[string]bool{}
	for i, line := range strings.Split(sharedDriveParameterProbe, "\n") {
		if strings.HasSuffix(line, "// want") {
			want[fmt.Sprintf("%s:%d", path, i+1)] = true
		}
	}

	found := map[string]bool{}
	for _, finding := range driveSharedParameterFailures(t, root, map[string][]byte{path: []byte(sharedDriveParameterProbe)}) {
		at, _, ok := strings.Cut(finding, ": ")
		if !ok {
			t.Errorf("finding has no location: %q", finding)
			continue
		}
		if !want[at] {
			t.Errorf("reported valid code: %s", finding)
			continue
		}
		found[at] = true
	}
	for at := range want {
		if !found[at] {
			t.Errorf("missed probe line %s", probeLine(map[string]string{path: sharedDriveParameterProbe}, at))
		}
	}
}

func driveSharedParameterFailures(t *testing.T, root string, overlay map[string][]byte) []string {
	t.Helper()
	fset, files, uses := typeCheckGDrive(t, root, overlay)
	parents := astParents(files)
	var findings []string
	for _, file := range files {
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			method, ok := driveFilesServiceCall(call, uses)
			if !ok {
				return true
			}

			required := []string{"SupportsAllDrives"}
			if method == "List" {
				required = append(required, "IncludeItemsFromAllDrives")
			}
			for _, parameter := range required {
				if sharedDriveParameterIsTrue(call, parents, parameter) {
					continue
				}
				pos := fset.Position(call.Pos())
				findings = append(findings, fmt.Sprintf("%s:%d: Files.%s call is missing %s(true)", pos.Filename, pos.Line, method, parameter))
			}
			return true
		})
	}
	sort.Strings(findings)
	return findings
}

func astParents(files []*ast.File) map[ast.Node]ast.Node {
	parents := map[ast.Node]ast.Node{}
	for _, file := range files {
		var stack []ast.Node
		ast.Inspect(file, func(node ast.Node) bool {
			if node == nil {
				stack = stack[:len(stack)-1]
				return false
			}
			if len(stack) > 0 {
				parents[node] = stack[len(stack)-1]
			}
			stack = append(stack, node)
			return true
		})
	}
	return parents
}

func driveFilesServiceCall(call *ast.CallExpr, uses map[*ast.Ident]types.Object) (string, bool) {
	selector, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	method, ok := uses[selector.Sel].(*types.Func)
	if !ok || method.Pkg() == nil || method.Pkg().Path() != drivePackage {
		return "", false
	}
	signature := method.Signature()
	recv := signature.Recv()
	if recv == nil {
		return "", false
	}
	receiver := namedType(recv.Type())
	if receiver == nil || receiver.Obj().Name() != "FilesService" {
		return "", false
	}
	if signature.Results().Len() == 0 || !hasMethod(signature.Results().At(0).Type(), "SupportsAllDrives") {
		return "", false
	}
	return method.Name(), true
}

func namedType(typ types.Type) *types.Named {
	if pointer, ok := typ.(*types.Pointer); ok {
		typ = pointer.Elem()
	}
	named, _ := types.Unalias(typ).(*types.Named)
	return named
}

func hasMethod(typ types.Type, method string) bool {
	methods := types.NewMethodSet(typ)
	for i := range methods.Len() {
		if methods.At(i).Obj().Name() == method {
			return true
		}
	}
	return false
}

func sharedDriveParameterIsTrue(root *ast.CallExpr, parents map[ast.Node]ast.Node, parameter string) bool {
	var current ast.Node = root
	for {
		parent := parents[current]
		if parens, ok := parent.(*ast.ParenExpr); ok && parens.X == current {
			current = parens
			continue
		}
		selector, ok := parent.(*ast.SelectorExpr)
		if !ok || selector.X != current {
			return false
		}
		call, ok := parents[selector].(*ast.CallExpr)
		if !ok || call.Fun != selector {
			return false
		}
		if selector.Sel.Name == parameter && callHasTrueArgument(call) {
			return true
		}
		current = call
	}
}

func callHasTrueArgument(call *ast.CallExpr) bool {
	if len(call.Args) != 1 {
		return false
	}
	value, ok := ast.Unparen(call.Args[0]).(*ast.Ident)
	return ok && value.Name == "true"
}
