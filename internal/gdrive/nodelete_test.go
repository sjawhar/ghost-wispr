package gdrive

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/api/drive/v3"

	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// Review of #37, round 4, blocker 1(a): the re-sync of a folder as main left
// it (summary.md and transcript.md with no content hash, audio.mp3), which
// the re-sync migration starts for every merged session at upgrade, deleted
// both documents for good: their revision history, comments and any edit the
// user made there with them. It now updates each in place: the same file, so
// the same id, comments and links, and Drive's revision history keeps the
// earlier text. Nothing is deleted.
func TestResyncOfAMainEraFolderUpdatesTheDocumentsInPlace(t *testing.T) {
	wire, syncer := newWireDrive(t)
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	folder := wire.add("root", "2026-11-06 Merged", "application/vnd.google-apps.folder", nil, nil)
	summaryID := wire.add(folder, "summary.md", "application/vnd.google-apps.document", []byte("the summary main uploaded, edited by hand"), nil)
	transcriptID := wire.add(folder, "transcript.md", "application/vnd.google-apps.document", []byte("the transcript main uploaded"), nil)
	audioID := wire.add(folder, "audio.mp3", "audio/mpeg", []byte("audio"), nil)

	recording := filepath.Join(t.TempDir(), "20261106100000.mp3")
	if err := os.WriteFile(recording, []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	const id = "20261106100000-merged"
	started := time.Now().UTC().Add(-time.Hour)
	if err := store.CreateSession(id, started); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := store.EndSession(id, started.Add(time.Minute), recording); err != nil {
		t.Fatalf("end session: %v", err)
	}
	if err := store.UpdateSummary(id, "Merged", "The current summary.", storage.SummaryCompleted, "default", "{}"); err != nil {
		t.Fatalf("summarize: %v", err)
	}
	// As the re-sync migration leaves it: pending, its folder kept.
	syncTo(t, store, id, storage.SyncStatePending, folder, 0, "")

	if err := NewOrchestrator(syncer, store).SyncSession(context.Background(), id); err != nil {
		t.Fatalf("re-sync: %v", err)
	}

	files := wire.in(folder)
	for name, wantID := range map[string]string{"summary.md": summaryID, "transcript.md": transcriptID, "audio.mp3": audioID} {
		if got := files[name]; len(got) != 1 || got[0].ID != wantID {
			var ids []string
			for _, f := range got {
				ids = append(ids, f.ID)
			}
			t.Errorf("%s: files %v, want the one file %s", name, ids, wantID)
		}
	}
	if docs := files["summary.md"]; len(docs) > 0 && !strings.Contains(string(docs[len(docs)-1].Content), "The current summary.") {
		t.Errorf("summary.md holds %q, want the current summary", docs[len(docs)-1].Content)
	}
	if len(files["audio-20261106100000.mp3"]) != 1 {
		t.Errorf("the recording was not uploaded under its own name: folder holds %v", keysOf(files))
	}
	if sess, err := store.GetSession(id); err != nil || sess.SyncState != storage.SyncStateSynced {
		t.Errorf("after the re-sync: %q (%v), want synced", sess.SyncState, err)
	}
}

func keysOf[V any](m map[string]V) []string {
	var names []string
	for name := range m {
		names = append(names, name)
	}
	return names
}

// A folder holding two copies of a document (an earlier version of this
// change could leave them) restores the newer one, whichever Drive lists
// last.
func TestRestoreReadsTheNewestCopyOfADocument(t *testing.T) {
	wire, syncer := newWireDrive(t)
	const folder = "folder-two-copies"
	started := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	render := func(text string) (summary, transcript []byte) {
		sess := &SyncSession{ID: "20261106100000", Title: "Standup", StartedAt: started, Summary: text, SummaryPreset: "default"}
		segments := []transcribe.Segment{{Text: text, StartTime: 0, EndTime: 1, Timestamp: started}}
		return []byte(RenderSummaryMarkdown(sess)), []byte(RenderTranscriptMarkdown(sess, segments))
	}
	newSummary, newTranscript := render("the newer text")
	oldSummary, oldTranscript := render("the older text")
	// Listed newer first, so the older is listed last.
	newer := []string{
		wire.add(folder, "summary.md", "application/vnd.google-apps.document", newSummary, nil),
		wire.add(folder, "transcript.md", "application/vnd.google-apps.document", newTranscript, nil),
	}
	wire.add(folder, "summary.md", "application/vnd.google-apps.document", oldSummary, nil)
	wire.add(folder, "transcript.md", "application/vnd.google-apps.document", oldTranscript, nil)
	wire.mu.Lock()
	for _, id := range newer {
		wire.find(id).ModifiedTime = started.Add(24 * time.Hour)
	}
	wire.mu.Unlock()

	restored, err := syncer.restoreFolder(context.Background(), &drive.File{Id: folder})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if !strings.Contains(restored.Summary, "the newer text") {
		t.Errorf("restored summary %q, want the newer copy's", restored.Summary)
	}
	if len(restored.Segments) != 1 || restored.Segments[0].Text != "the newer text" {
		t.Errorf("restored segments %+v, want the newer copy's", restored.Segments)
	}
}

// moduleRoot returns the directory of the module's go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root %s: %v", root, err)
	}
	return root
}

// drivePackage is the Drive API package whose deletes driveDeletions finds.
const drivePackage = "google.golang.org/api/drive/v3"

// goPackage is a package as go list reports it.
type goPackage struct {
	ImportPath, Dir, Export  string
	GoFiles, CompiledGoFiles []string
	DepOnly                  bool
	Error                    *struct{ Err string }
}

// moduleList is go list's report on the module's packages (and the export
// data of every package they import), made once per test process: it builds
// them, and overlay files do not change it.
var moduleList struct {
	once     sync.Once
	packages []goPackage
	exports  map[string]string
	err      error
}

// listModule returns the packages of the module at root, and the export data
// file of every package they import, by import path.
func listModule(root string) ([]goPackage, map[string]string, error) {
	moduleList.once.Do(func() {
		var stderr bytes.Buffer
		cmd := exec.Command("go", "list", "-e", "-export", "-compiled", "-deps", "-json=ImportPath,Dir,Export,GoFiles,CompiledGoFiles,DepOnly,Error", "./...")
		cmd.Dir, cmd.Stderr = root, &stderr
		out, err := cmd.Output()
		if err != nil {
			moduleList.err = fmt.Errorf("go list: %w\n%s", err, stderr.Bytes())
			return
		}
		moduleList.exports = map[string]string{}
		for dec := json.NewDecoder(bytes.NewReader(out)); dec.More(); {
			var p goPackage
			if err := dec.Decode(&p); err != nil {
				moduleList.err = fmt.Errorf("read go list: %w", err)
				return
			}
			// The app embeds the web build (cmd/ghost-wispr/static), which a
			// checkout has only once the frontend is built; its Go files are
			// checked all the same.
			embedOnly := p.Error != nil && !p.DepOnly && strings.Contains(p.Error.Err, "no matching files found")
			if p.Error != nil && !embedOnly {
				moduleList.err = fmt.Errorf("go list %s: %s", p.ImportPath, p.Error.Err)
				return
			}
			if len(p.CompiledGoFiles) == 0 {
				p.CompiledGoFiles = p.GoFiles
			}
			moduleList.exports[p.ImportPath] = p.Export
			if !p.DepOnly {
				moduleList.packages = append(moduleList.packages, p)
			}
		}
	})
	return moduleList.packages, moduleList.exports, moduleList.err
}

// typeCheckModule type-checks each package of the module at root, its code
// outside tests with overlay's files (path to content) added, and returns its
// files and type information. Imports are read from the export data go list
// builds, with the standard library's importer, which reads the format of the
// toolchain running the test.
func typeCheckModule(t *testing.T, root string, overlay map[string][]byte) (*token.FileSet, []*ast.File, *types.Info) {
	t.Helper()
	modulePackages, exports, err := listModule(root)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	imports := importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		if exports[path] == "" {
			return nil, fmt.Errorf("no export data for %s", path)
		}
		return os.Open(exports[path])
	})
	var all []*ast.File
	info := &types.Info{
		Uses:  map[*ast.Ident]types.Object{},
		Types: map[ast.Expr]types.TypeAndValue{},
	}
	for _, p := range modulePackages {
		sources := map[string]any{}
		for _, name := range p.CompiledGoFiles {
			if !filepath.IsAbs(name) {
				name = filepath.Join(p.Dir, name)
			}
			sources[name] = nil
		}
		for path, src := range overlay {
			if filepath.Dir(path) == p.Dir {
				sources[path] = src
			}
		}
		var files []*ast.File
		for path, src := range sources {
			file, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			files = append(files, file)
		}
		conf := types.Config{Importer: imports, Error: func(err error) {
			t.Errorf("package %s does not type-check, so the check cannot see it: %v", p.ImportPath, err)
		}}
		_, _ = conf.Check(p.ImportPath, fset, files, info)
		all = append(all, files...)
	}
	if t.Failed() {
		t.FailNow()
	}
	if len(all) < 20 {
		t.Fatalf("type-checked %d Go files under %s; the check missed the code", len(all), root)
	}
	return fset, all, info
}

// written returns the identifiers in files that a value is written through:
// the left of an assignment, the key of a composite literal, and an operand
// whose address is taken (a write may follow through the pointer).
func written(files []*ast.File) map[*ast.Ident]bool {
	ids := map[*ast.Ident]bool{}
	mark := func(e ast.Expr) {
		switch e := ast.Unparen(e).(type) {
		case *ast.Ident:
			ids[e] = true
		case *ast.SelectorExpr:
			ids[e.Sel] = true
		}
	}
	for _, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.AssignStmt:
				for _, lhs := range n.Lhs {
					mark(lhs)
				}
			case *ast.KeyValueExpr:
				mark(n.Key)
			case *ast.UnaryExpr:
				if n.Op == token.AND {
					mark(n.X)
				}
			}
			return true
		})
	}
	return ids
}

// driveDeletions returns each place in the module's Go code outside tests,
// with overlay's files (path to content) added, that could delete or trash
// something on Drive through the Drive v3 client the app uses, as
// "file:line: what". It type-checks the code, so it finds every use of a
// method of that client that deletes (any Delete, of a file, a revision, a
// comment or a permission, and EmptyTrash) and of the call such a method
// returns, and every write of a file's Trashed field, however the code
// reaches it: directly, through an aliased service, a method value, an
// interface or an embedded field. A request made without that client (raw
// HTTP, another API version) is not seen.
func driveDeletions(t *testing.T, root string, overlay map[string][]byte) []string {
	t.Helper()
	fset, files, info := typeCheckModule(t, root, overlay)
	writes := written(files)
	var found []string
	driveUses := 0
	for id, obj := range info.Uses {
		if obj.Pkg() == nil || obj.Pkg().Path() != drivePackage {
			continue
		}
		driveUses++
		if what := driveDeletion(obj, writes[id]); what != "" {
			pos := fset.Position(id.Pos())
			found = append(found, fmt.Sprintf("%s:%d: %s", pos.Filename, pos.Line, what))
		}
	}
	if driveUses == 0 {
		t.Fatalf("no code under %s uses %s: the check sees none of it", root, drivePackage)
	}
	sort.Strings(found)
	return found
}

// driveDeletion says how obj, part of the Drive API, deletes or trashes
// something on Drive, or "" if it does not; written is whether this use of
// obj writes it.
func driveDeletion(obj types.Object, written bool) string {
	switch obj := obj.(type) {
	case *types.Func:
		recv := obj.Signature().Recv()
		if recv == nil {
			return ""
		}
		on := recv.Type()
		if ptr, ok := on.(*types.Pointer); ok {
			on = ptr.Elem()
		}
		named, ok := types.Unalias(on).(*types.Named)
		if !ok {
			return ""
		}
		recvName := named.Obj().Name()
		switch {
		case obj.Name() == "Delete" || obj.Name() == "EmptyTrash":
			return fmt.Sprintf("%s.%s deletes on Drive", recvName, obj.Name())
		case isDriveDeleteCall(recvName):
			return fmt.Sprintf("%s.%s runs a delete on Drive", recvName, obj.Name())
		}
	case *types.TypeName:
		if isDriveDeleteCall(obj.Name()) {
			return fmt.Sprintf("%s is a delete on Drive", obj.Name())
		}
	case *types.Var:
		if obj.IsField() && obj.Name() == "Trashed" && written {
			return "sets a Drive file's Trashed field, which trashes it"
		}
	}
	return ""
}

// isDriveDeleteCall reports whether name is the Drive API's type for a call
// that deletes (FilesDeleteCall, FilesEmptyTrashCall, RevisionsDeleteCall).
func isDriveDeleteCall(name string) bool {
	return strings.HasSuffix(name, "DeleteCall") || strings.HasSuffix(name, "EmptyTrashCall")
}

// No app code deletes or trashes a Drive file except ReplaceSessionFiles,
// which removes the three old files only after their replacements are stored.
func TestNoCodeDeletesOrTrashesDriveFiles(t *testing.T) {
	root := moduleRoot(t)
	replacePath := filepath.Join(root, "internal", "gdrive", "replace.go")
	allowedLocations := map[string]bool{}
	for _, finding := range driveDeletions(t, root, nil) {
		path, line, ok := driveDeletionLocation(finding)
		if ok && path == replacePath && functionAtLine(t, path, line) == "ReplaceSessionFiles" {
			allowedLocations[fmt.Sprintf("%s:%d", path, line)] = true
			continue
		}
		t.Error(finding)
	}
	if len(allowedLocations) != 1 {
		t.Errorf("found %d allowed Drive delete call sites in ReplaceSessionFiles, want exactly one", len(allowedLocations))
	}
}

func driveDeletionLocation(finding string) (string, int, bool) {
	path, rest, ok := strings.Cut(finding, ".go:")
	if !ok {
		return "", 0, false
	}
	lineText, _, ok := strings.Cut(rest, ":")
	if !ok {
		return "", 0, false
	}
	line, err := strconv.Atoi(lineText)
	if err != nil {
		return "", 0, false
	}
	return path + ".go", line, true
}

func functionAtLine(t *testing.T, path string, line int) string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok {
			continue
		}
		start := fset.Position(function.Pos()).Line
		end := fset.Position(function.End()).Line
		if start <= line && line <= end {
			return function.Name.Name
		}
	}
	return ""
}

// deletionProbe is code that deletes or trashes files on Drive in every form
// driveDeletions must find; each line it must report ends in "// want", and
// a line without it (reading Trashed) must not be reported.
const deletionProbe = `package gdrive

import "google.golang.org/api/drive/v3"

func (s *Syncer) probeDeleteThroughAnAlias(id string) error {
	files := s.service.Files
	return files.Delete(id).Do() // want
}

func (s *Syncer) probeDeleteThroughAMethodValue(id string) error {
	remove := s.service.Files.Delete // want
	return remove(id).Do() // want
}

type probeDeleter interface {
	Delete(fileID string) *drive.FilesDeleteCall // want
}

func (s *Syncer) probeDeleteThroughAnInterface(id string) error {
	var d probeDeleter = s.service.Files
	return d.Delete(id).Do() // want
}

func (s *Syncer) probeEmptyTrashThroughAnAliasedService() error {
	svc := s.service
	return svc.Files.EmptyTrash().Do() // want
}

func (s *Syncer) probeDeleteARevision(fileID, revisionID string) error {
	return s.service.Revisions.Delete(fileID, revisionID).Do() // want
}

func (s *Syncer) probeTrashByAssignment(id string) error {
	f := &drive.File{}
	f.Trashed = true // want
	_, err := s.service.Files.Update(id, f).Do()
	return err
}

func (s *Syncer) probeTrashByKey(id string) error {
	_, err := s.service.Files.Update(id, &drive.File{Trashed: true}).Do() // want
	return err
}

func (s *Syncer) probeTrashThroughAPointer(id string) error {
	f := &drive.File{}
	trashed := &f.Trashed // want
	*trashed = true
	_, err := s.service.Files.Update(id, f).Do()
	return err
}

func probeReadTrashed(f *drive.File) bool {
	return f.Trashed
}
`

// storageProbe deletes a file on Drive from another package of the module,
// through a service it is handed.
const storageProbe = `package storage

import "google.golang.org/api/drive/v3"

func probeDeleteFromAnotherPackage(s *drive.Service, id string) error {
	return s.Files.Delete(id).Do() // want
}
`

// Review of #37, round 5, note 2 (red-team N1): the check missed Trashed set
// by an assignment and a delete through an aliased Drive service. It now
// finds every form in the probes, in any package, and nothing else there.
func TestTheDriveDeletionCheckFindsEveryForm(t *testing.T) {
	root := moduleRoot(t)
	probes := map[string]string{
		filepath.Join(root, "internal", "gdrive", "zz_deletion_probe.go"):  deletionProbe,
		filepath.Join(root, "internal", "storage", "zz_deletion_probe.go"): storageProbe,
	}
	overlay := map[string][]byte{}
	want := map[string]bool{} // "path:line"
	for path, src := range probes {
		overlay[path] = []byte(src)
		for i, line := range strings.Split(src, "\n") {
			if strings.HasSuffix(line, "// want") {
				want[fmt.Sprintf("%s:%d", path, i+1)] = true
			}
		}
	}
	found := map[string]bool{}
	replacePath := filepath.Join(root, "internal", "gdrive", "replace.go")
	for _, finding := range driveDeletions(t, root, overlay) {
		path, line, ok := driveDeletionLocation(finding)
		if !ok {
			t.Fatalf("finding %q has no source location", finding)
		}
		if path == replacePath && functionAtLine(t, path, line) == "ReplaceSessionFiles" {
			continue
		}
		at := fmt.Sprintf("%s:%d", path, line)
		if _, ok := probes[path]; !ok {
			t.Errorf("outside the probes: %s", finding)
			continue
		}
		found[at] = true
	}
	for at := range want {
		if !found[at] {
			t.Errorf("missed probe line %s", probeLine(probes, at))
		}
		delete(found, at)
	}
	for at := range found {
		t.Errorf("reported probe line %s, which deletes nothing", probeLine(probes, at))
	}
}

// probeLine is "path:line: text" for at ("path:line") in probes.
func probeLine(probes map[string]string, at string) string {
	i := strings.LastIndex(at, ":")
	line, _ := strconv.Atoi(at[i+1:])
	lines := strings.Split(probes[at[:i]], "\n")
	return fmt.Sprintf("%s: %s", at, strings.TrimSpace(lines[line-1]))
}

// Review of #37, round 4, blocker 1(b): two syncs of one session could run at
// once (retry-sync, the sync after a summary, the sweep). The one that read
// the session first could upload last: it replaced the current summary on
// Drive with the old one, and the session was marked synced anyway, so no
// later sync put it right. Now the sync after the summary finds the session
// claimed by the running sync and starts nothing; the running sync, which
// uploaded the old summary, ends in a retry, and the retry converges on the
// current one.
func TestAStaleSyncNeverLeavesAnOldSummarySynced(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	recording := filepath.Join(t.TempDir(), "20261106100000.mp3")
	if err := os.WriteFile(recording, []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	const id = "20261106100000"
	started := time.Now().UTC().Add(-time.Hour)
	if err := store.CreateSession(id, started); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := store.EndSession(id, started.Add(time.Minute), recording); err != nil {
		t.Fatalf("end session: %v", err)
	}
	if err := store.UpdateSummary(id, "", "The first summary.", storage.SummaryCompleted, "default", "{}"); err != nil {
		t.Fatalf("summarize: %v", err)
	}
	drive := newFakeDrive()
	o := NewOrchestratorWithUploader(drive, store)
	ctx := context.Background()

	// The first sync lands both documents, then fails on the recording.
	drive.failAfter = 2
	if err := o.SyncSession(ctx, id); err == nil {
		t.Fatal("the cut-off first sync succeeded")
	}
	drive.failAfter = 0

	// The retry reads the session; as it starts uploading, the session is
	// summarized again and the sync after the summary is started.
	var fired atomic.Bool
	drive.onUpload = func() {
		if !fired.CompareAndSwap(false, true) {
			return
		}
		if err := store.UpdateSummary(id, "", "The summary, made again.", storage.SummaryCompleted, "default", "{}"); err != nil {
			t.Errorf("summarize again: %v", err)
		}
		if err := o.SyncSession(ctx, id); err != nil {
			t.Errorf("the sync after the summary: %v", err)
		}
	}
	staleErr := o.SyncSession(ctx, id)
	drive.onUpload = nil

	sess, err := store.GetSession(id)
	if err != nil {
		t.Fatalf("read session: %v", err)
	}
	summary := string(drive.files(sess.GDriveFolderID)["summary.md"])
	if sess.SyncState == storage.SyncStateSynced && !strings.Contains(summary, "The summary, made again.") {
		t.Errorf("the stale sync (%v) left the session synced with the old summary on Drive:\n%s", staleErr, summary)
	}

	if sess.SyncState != storage.SyncStateSynced {
		if err := o.SyncSession(ctx, id); err != nil {
			t.Fatalf("retry: %v", err)
		}
	}
	if sess, err = store.GetSession(id); err != nil || sess.SyncState != storage.SyncStateSynced {
		t.Fatalf("after the retry: %q (%v), want synced", sess.SyncState, err)
	}
	summaries := 0
	for _, f := range drive.folders[sess.GDriveFolderID] {
		if f.Name == "summary.md" {
			summaries++
			if !strings.Contains(string(f.content), "The summary, made again.") {
				t.Errorf("synced, with Drive's summary.md:\n%s\nwant the summary made again", f.content)
			}
		}
	}
	if summaries != 1 {
		t.Errorf("the folder holds %d summary.md, want 1", summaries)
	}
}
