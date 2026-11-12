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

	"google.golang.org/api/drive/v3"

	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// TestEveryDriveCallSetsSupportsAllDrives keeps every Drive API request that
// supports shared drives configured for the shared drive the application uses.
func TestEveryDriveCallSetsSupportsAllDrives(t *testing.T) {
	for _, finding := range driveSharedParameterFailures(t, moduleRoot(t), nil) {
		t.Error(finding)
	}
}

// A sync into a shared drive's root, as the drive.file scope sees it:
// files.get answers 404 for the root, so neither the first sync, which makes
// the session's folder there, nor a re-sync, which updates that folder in
// place, may depend on reading the root.
func TestASyncIntoASharedDriveRoot(t *testing.T) {
	wire, syncer := newSharedDriveWire(t)
	store := openStore(t)
	o := NewOrchestrator(syncer, store)
	const id = "20261111090000"
	summarizedSession(t, store, t.TempDir(), id, time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC), "The summary.")
	folder := syncedSession(t, store, o, id)

	if err := store.UpdateTitle(id, "Renamed"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := o.SyncSession(context.Background(), id); err != nil {
		t.Fatalf("re-sync into the shared drive: %v", err)
	}
	if state, kept := sessionState(t, store, id); state != storage.SyncStateSynced || kept != folder {
		t.Errorf("after the re-sync: %q in folder %q, want %q in %q", state, kept, storage.SyncStateSynced, folder)
	}
	if folders := wire.in(sharedDriveID); len(folders) != 1 {
		t.Errorf("shared drive root holds %v, want the session's one folder", keysOf(folders))
	}
	summaries := wire.in(folder)["summary.md"]
	if len(summaries) != 1 || !strings.Contains(string(summaries[0].Content), "Renamed") {
		t.Errorf("summary.md: %d copies, want one updated in place to the new title", len(summaries))
	}
}

func TestRestoreFromDriveListsSharedDriveFolders(t *testing.T) {
	wire, syncer := newSharedDriveWire(t)

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

// A restore lists a shared drive's session folders without corpora, a list
// Drive documents as holding the folders the app made there (not measured
// with folders there). Should it leave them out, a restore that lists none
// while the drive's own list (corpora "drive") holds one fails, instead of
// reporting an empty Drive. An empty drive still restores none, with no
// error.
func TestRestoreFailsWhenTheListWithoutCorporaMissesTheDrivesFolders(t *testing.T) {
	wire, syncer := newSharedDriveWire(t)
	wire.unscopedListsMiss = true
	importNone := func(RestoredSession) error { return nil }

	result, err := syncer.RestoreFromDrive(context.Background(), importNone)
	if err != nil || result.Restored != 0 || len(result.Errors) != 0 {
		t.Errorf("restore from an empty shared drive: %+v, %v; want none restored and no error", result, err)
	}
	folder := wire.add(sharedDriveID, "2026-11-10-standup", "application/vnd.google-apps.folder", nil, nil)
	result, err = syncer.RestoreFromDrive(context.Background(), importNone)
	if err == nil || !strings.Contains(err.Error(), folder) {
		t.Errorf("restore with folder %s left out of the list without corpora: %+v, %v; want an error naming it", folder, result, err)
	}
}

// A restore that lists no session folder in a shared drive that reads asks
// the drive's own list for a folder; when that list does not answer, Drive
// has not said the drive is empty, so the restore fails instead of
// reporting none.
func TestRestoreFailsWhenTheDrivesFolderListDoesNotAnswer(t *testing.T) {
	wire, syncer := newSharedDriveWire(t)
	wire.driveFolderListUnavailable = true

	result, err := syncer.RestoreFromDrive(context.Background(), func(RestoredSession) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "could not be listed") {
		t.Errorf("restore with the drive's folder list not answering: %+v, %v; want an error", result, err)
	}
}

// The folder the app syncs into is set by hand, so a quote in its id is
// escaped in the query that lists its session folders, as in the Syncer's
// other queries: unescaped, Drive cannot read the query (400), and the
// restore fails.
func TestRestoreFromARootWhoseIDHasAQuote(t *testing.T) {
	for _, root := range wireRoots {
		t.Run(root.name, func(t *testing.T) {
			wire, syncer := root.newWire(t)
			const quoted = "root'with-a-quote"
			wire.rootID, syncer.folderID = quoted, quoted
			started := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
			folder := wire.add(quoted, "2026-11-10-standup", "application/vnd.google-apps.folder", nil, nil)
			transcript := RenderTranscriptMarkdown(&SyncSession{ID: "20261110090000", StartedAt: started}, []transcribe.Segment{{
				Speaker: 0, Text: "a quoted root", Timestamp: started,
			}})
			wire.add(folder, "transcript.md", "application/vnd.google-apps.document", []byte(transcript), nil)

			result, err := syncer.RestoreFromDrive(context.Background(), func(RestoredSession) error { return nil })
			if err != nil || result.Restored != 1 || len(result.Errors) != 0 {
				t.Errorf("restore from root %q: %+v, %v; want one restored session and no errors", quoted, result, err)
			}
		})
	}
}

func TestRestoreFromDrivePaginatesSharedDriveLists(t *testing.T) {
	wire, syncer := newSharedDriveWire(t)

	started := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	transcript := func(id, text string) []byte {
		return []byte(RenderTranscriptMarkdown(&SyncSession{ID: id, StartedAt: started}, []transcribe.Segment{{
			Speaker:   0,
			Text:      text,
			Timestamp: started,
		}}))
	}
	file := func(id, name, mimeType string) *drive.File {
		return &drive.File{Id: id, Name: name, MimeType: mimeType}
	}
	folderOne := wire.add(sharedDriveID, "2026-11-10-first", "application/vnd.google-apps.folder", nil, nil)
	folderTwo := wire.add(sharedDriveID, "2026-11-10-second", "application/vnd.google-apps.folder", nil, nil)
	transcriptOne := wire.add(folderOne, "transcript.md", "application/vnd.google-apps.document", transcript("20261110090000", "first page"), nil)
	audioOne := wire.add(folderOne, "audio.mp3", "audio/mpeg", []byte("audio"), nil)
	transcriptTwo := wire.add(folderTwo, "transcript.md", "application/vnd.google-apps.document", transcript("20261110090100", "second page"), nil)
	wire.listPages = map[string]map[string]*drive.FileList{
		sharedDriveID: {
			"":          {Files: []*drive.File{file(folderOne, "2026-11-10-first", "application/vnd.google-apps.folder")}, NextPageToken: "folders-2"},
			"folders-2": {Files: []*drive.File{file(folderTwo, "2026-11-10-second", "application/vnd.google-apps.folder")}},
		},
		folderOne: {
			"":        {Files: []*drive.File{file(transcriptOne, "transcript.md", "application/vnd.google-apps.document")}, NextPageToken: "files-2"},
			"files-2": {Files: []*drive.File{file(audioOne, "audio.mp3", "audio/mpeg")}},
		},
		folderTwo: {
			"": {Files: []*drive.File{file(transcriptTwo, "transcript.md", "application/vnd.google-apps.document")}},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	imported := make(chan RestoredSession, 2)
	result, err := syncer.RestoreFromDrive(ctx, func(sess RestoredSession) error {
		imported <- sess
		return nil
	})
	if err != nil {
		t.Fatalf("restore paginated drive: %v", err)
	}
	if result.Restored != 2 || len(result.Errors) != 0 {
		t.Fatalf("restore result: %+v, want two restored sessions and no errors", result)
	}
	sessions := map[string]RestoredSession{}
	for range 2 {
		sess := <-imported
		sessions[sess.ID] = sess
	}
	if !sessions["20261110090000"].HasAudio {
		t.Error("first session lost the audio from its second file page")
	}
	for _, request := range []wireListRequest{
		{Parent: sharedDriveID},
		{Parent: sharedDriveID, PageToken: "folders-2"},
		{Parent: folderOne},
		{Parent: folderOne, PageToken: "files-2"},
		{Parent: folderTwo},
	} {
		if !wire.listed(request) {
			t.Errorf("Drive did not request page %+v", request)
		}
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

func (s *Syncer) probeGetWithLaterSupportsAllDrivesFalse(id string) error {
	call := s.service.Files.Get(id) // want SupportsAllDrives
	call = call.SupportsAllDrives(false) // want SupportsAllDrives
	_, err := call.Do()
	return err
}

func (s *Syncer) probeListWithLaterIncludeItemsFromAllDrivesFalse() error {
	call := s.service.Files.List().SupportsAllDrives(true) // want IncludeItemsFromAllDrives
	call = call.IncludeItemsFromAllDrives(false) // want IncludeItemsFromAllDrives
	_, err := call.Do()
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
		key, _, ok := strings.Cut(finding, ": ")
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
			if parameter, ok := sharedDriveParameterMethodCall(call, info.Uses); ok && !callHasTrueArgument(call) {
				pos := fset.Position(call.Pos())
				findings = append(findings, fmt.Sprintf("%s:%d:%s: non-true argument to %s", pos.Filename, pos.Line, parameter, parameter))
			}
			callType, ok := driveCallType(info.Types[call].Type)
			if !ok || continuesDriveCall(call, callType, info.Uses) {
				return true
			}

			driveCalls++
			for _, parameter := range sharedDriveParameters(callType) {
				if sharedDriveParameterIsTrue(call, parents, parameter) || sharedDriveParameterIsPresent(call, parents, parameter) {
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

func sharedDriveParameterMethodCall(call *ast.CallExpr, uses map[*ast.Ident]types.Object) (string, bool) {
	selector, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	parameter := selector.Sel.Name
	if parameter != "SupportsAllDrives" && parameter != "IncludeItemsFromAllDrives" {
		return "", false
	}
	method, ok := uses[selector.Sel].(*types.Func)
	if !ok {
		return "", false
	}
	recv := method.Signature().Recv()
	if recv == nil {
		return "", false
	}
	callType, ok := driveCallType(recv.Type())
	return parameter, ok && hasMethod(callType, parameter)
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
	call := sharedDriveParameterInChain(root, parents, parameter)
	return call != nil && callHasTrueArgument(call)
}

func sharedDriveParameterIsPresent(root *ast.CallExpr, parents map[ast.Node]ast.Node, parameter string) bool {
	return sharedDriveParameterInChain(root, parents, parameter) != nil
}

func sharedDriveParameterInChain(root *ast.CallExpr, parents map[ast.Node]ast.Node, parameter string) *ast.CallExpr {
	var current ast.Node = root
	for {
		parent := parents[current]
		if parens, ok := parent.(*ast.ParenExpr); ok && parens.X == current {
			current = parens
			continue
		}
		selector, ok := parent.(*ast.SelectorExpr)
		if !ok || selector.X != current {
			return nil
		}
		call, ok := parents[selector].(*ast.CallExpr)
		if !ok || call.Fun != selector {
			return nil
		}
		if selector.Sel.Name == parameter {
			return call
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
