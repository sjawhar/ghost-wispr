package gdrive

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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

	const folder = "folder-main-era"
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
	if err := store.UpdateSyncState(id, storage.SyncStatePending, folder, 0, nil, ""); err != nil {
		t.Fatalf("pending: %v", err)
	}

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

// Nothing in the app deletes or trashes a file on Google Drive: Drive holds
// the user's backup, and a document there may hold their edits. Every Go
// file outside tests is checked for Files.Delete, Files.EmptyTrash and a
// Trashed field.
func TestNoCodeDeletesOrTrashesDriveFiles(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root %s: %v", root, err)
	}
	checked := 0
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "node_modules", "web", "vendor", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		checked++
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.SelectorExpr:
				if inner, ok := n.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "Files" && (n.Sel.Name == "Delete" || n.Sel.Name == "EmptyTrash") {
					t.Errorf("%s: Files.%s deletes files on Drive", fset.Position(n.Pos()), n.Sel.Name)
				}
			case *ast.KeyValueExpr:
				if key, ok := n.Key.(*ast.Ident); ok && key.Name == "Trashed" {
					t.Errorf("%s: sets Trashed on a Drive file", fset.Position(n.Pos()))
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked < 20 {
		t.Fatalf("checked %d Go files under %s; the walk missed the code", checked, root)
	}
}

// Review of #37, round 4, blocker 1(b): two syncs of one session can run at
// once (retry-sync, the sync after a summary, the sweep). The one that read
// the session first could upload last: it replaced the current summary on
// Drive with the old one, and the session was marked synced anyway, so no
// later sync put it right. Now the stale sync either leaves the current
// summary on Drive or ends in a retry, and the retry converges on it.
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
	// summarized again and the sync after the summary runs to its end first.
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
