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
	"time"

	"github.com/sjawhar/ghost-wispr/internal/transcribe"
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

func TestRestoreFromDriveListsSharedDriveFolders(t *testing.T) {
	wire, syncer := newWireDrive(t)
	const sharedDriveID = "shared-drive-root"
	wire.rootID = sharedDriveID
	syncer.folderID = sharedDriveID

	started := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	folderID := wire.add(sharedDriveID, "2026-11-10-standup", "application/vnd.google-apps.folder", nil, nil)
	transcript := RenderTranscriptMarkdown(&SyncSession{ID: "20261110090000", StartedAt: started}, []transcribe.Segment{{
		Speaker:   0,
		Text:      "shared drive listing",
		Timestamp: started,
	}})
	wire.add(folderID, "transcript.md", "application/vnd.google-apps.document", []byte(transcript), nil)

	result, err := syncer.RestoreFromDrive(context.Background(), func(RestoredSession) error { return nil })
	if err != nil {
		t.Fatalf("restore from shared drive: %v", err)
	}
	if result.Restored != 1 || len(result.Errors) != 0 {
		t.Fatalf("restore result: %+v, want one restored session and no errors", result)
	}
}

const sharedDriveParameterProbe = `package gdrive

import "google.golang.org/api/drive/v3"

func (s *Syncer) probeGetWithoutSupportsAllDrives(id string) error {
	_, err := s.service.Files.Get(id).Do() // want SupportsAllDrives
	return err
}

func (s *Syncer) probeGetWithSupportsAllDrivesFalse(id string) error {
	_, err := s.service.Files.Get(id).SupportsAllDrives(false).Do() // want SupportsAllDrives
	return err
}

func (s *Syncer) probeListWithoutIncludeItemsFromAllDrives() error {
	_, err := s.service.Files.List().SupportsAllDrives(true).Do() // want IncludeItemsFromAllDrives
	return err
}

func (s *Syncer) probeListWithIncludeItemsFromAllDrivesFalse() error {
	_, err := s.service.Files.List().SupportsAllDrives(true).IncludeItemsFromAllDrives(false).Do() // want IncludeItemsFromAllDrives
	return err
}

func (s *Syncer) probeGetThroughAMethodValue(id string) error {
	get := s.service.Files.Get
	_, err := get(id).Do() // want SupportsAllDrives
	return err
}

type sharedDriveProbeGetter interface {
	Get(fileID string) *drive.FilesGetCall
}

func (s *Syncer) probeGetThroughAnInterface(id string) error {
	var getter sharedDriveProbeGetter = s.service.Files
	_, err := getter.Get(id).Do() // want SupportsAllDrives
	return err
}

func (s *Syncer) probeChangesListWithoutSharedDriveParameters() error {
	_, err := s.service.Changes.List("token").Do() // want SupportsAllDrives IncludeItemsFromAllDrives
	return err
}

func (s *Syncer) probePermissionsCreateWithoutSupportsAllDrives(id string) error {
	_, err := s.service.Permissions.Create(id, &drive.Permission{Type: "user", Role: "reader"}).Do() // want SupportsAllDrives
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

const sharedDriveParameterStorageProbe = `package storage

import "google.golang.org/api/drive/v3"

func probeDriveCallOutsideGDrive(s *drive.Service, id string) error {
	_, err := s.Files.Get(id).Do() // want SupportsAllDrives
	return err
}
`

// TestTheSharedDriveParameterCheckFindsEveryForm is a planted-probe control:
// it proves that the static guard finds omitted and false shared-drive
// parameters on every Drive call-chain form while leaving valid calls and
// Files.Export (which has no SupportsAllDrives option) alone.
func TestTheSharedDriveParameterCheckFindsEveryForm(t *testing.T) {
	root := moduleRoot(t)
	probes := map[string]string{
		filepath.Join(root, "internal", "gdrive", "zz_shared_drive_probe.go"):  sharedDriveParameterProbe,
		filepath.Join(root, "internal", "storage", "zz_shared_drive_probe.go"): sharedDriveParameterStorageProbe,
	}
	overlay := map[string][]byte{}
	want := map[string]bool{} // "path:line:parameter"
	for path, source := range probes {
		overlay[path] = []byte(source)
		for i, line := range strings.Split(source, "\n") {
			_, parameters, ok := strings.Cut(line, "// want ")
			if !ok {
				continue
			}
			for _, parameter := range strings.Fields(parameters) {
				want[fmt.Sprintf("%s:%d:%s", path, i+1, parameter)] = true
			}
		}
	}

	found := map[string]bool{}
	for _, finding := range driveSharedParameterFailures(t, root, overlay) {
		key, _, ok := strings.Cut(finding, ": missing ")
		if !ok {
			t.Errorf("finding has no path:line:parameter: %q", finding)
			continue
		}
		if !want[key] {
			t.Errorf("reported valid code: %s", finding)
			continue
		}
		found[key] = true
	}
	for key := range want {
		if !found[key] {
			t.Errorf("missed probe %s", key)
		}
	}
}

func driveSharedParameterFailures(t *testing.T, root string, overlay map[string][]byte) []string {
	t.Helper()
	fset, files, info := typeCheckModule(t, root, overlay)
	parents := astParents(files)
	var findings []string
	driveCalls := 0
	for _, file := range files {
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			callType, ok := driveCallType(info.Types[call].Type)
			if !ok || continuesDriveCall(call, callType, info.Uses) {
				return true
			}

			driveCalls++
			for _, parameter := range sharedDriveParameters(callType) {
				if sharedDriveParameterIsTrue(call, parents, parameter) {
					continue
				}
				pos := fset.Position(call.Pos())
				findings = append(findings, fmt.Sprintf("%s:%d:%s: missing %s(true)", pos.Filename, pos.Line, parameter, parameter))
			}
			return true
		})
	}
	if driveCalls == 0 {
		t.Fatalf("no code under %s makes a Drive call with SupportsAllDrives: the check sees none of it", root)
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

func driveCallType(typ types.Type) (types.Type, bool) {
	if typ == nil || !hasMethod(typ, "SupportsAllDrives") {
		return nil, false
	}
	named := namedType(typ)
	if named == nil || named.Obj().Pkg() == nil || named.Obj().Pkg().Path() != drivePackage {
		return nil, false
	}
	return typ, true
}

func continuesDriveCall(call *ast.CallExpr, callType types.Type, uses map[*ast.Ident]types.Object) bool {
	selector, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
	if !ok {
		return false
	}
	method, ok := uses[selector.Sel].(*types.Func)
	if !ok {
		return false
	}
	recv := method.Signature().Recv()
	return recv != nil && types.Identical(recv.Type(), callType)
}

func sharedDriveParameters(callType types.Type) []string {
	parameters := []string{"SupportsAllDrives"}
	if hasMethod(callType, "IncludeItemsFromAllDrives") {
		parameters = append(parameters, "IncludeItemsFromAllDrives")
	}
	return parameters
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
