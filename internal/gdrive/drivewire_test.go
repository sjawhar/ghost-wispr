package gdrive

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
)

// wireFile is a file in wireDrive.
type wireFile struct {
	ID, Name, MimeType string
	Parent             string
	AppProperties      map[string]string
	ModifiedTime       time.Time
	Content            []byte
	RevisionCount      int
}

type wireListRequest struct {
	Parent, PageToken string
}

func (d *wireDrive) listed(want wireListRequest) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, got := range d.listRequests {
		if got == want {
			return true
		}
	}
	return false
}

// wireDrive is the part of the Google Drive v3 HTTP API the Syncer uses,
// served over httptest, so the real Syncer (its requests on the wire) runs
// against it. It fails the test on any request that deletes or trashes a
// file, does not configure a shared-drive-capable operation for all drives,
// or that it does not know.
type wireDrive struct {
	t            *testing.T
	mu           sync.Mutex
	files        []*wireFile // in listing order
	listPages    map[string]map[string]*drive.FileList
	listRequests []wireListRequest
	next         int
	clock        time.Time
	rootID       string
	// sharedDrive makes rootID a shared drive's root, as Drive answers for
	// one under the app's drive.file scope (measured through the app on the
	// office Pi on 2026-11-11): files.get answers 404 for it, as for an id
	// Drive does not have, since the app did not create it; a list of its
	// children without corpora answers 200, with none for an id Drive does
	// not have; and a list with corpora "drive" and its driveId answers 200
	// while the app is a member, and 404 "Shared drive not found" for an id
	// Drive does not have. That the list without corpora answers the files
	// the app made there is Drive's documented corpora "user" (files the
	// user has accessed, shared drives' too), not measured: it answered no
	// files for the real drive then. The root deleted by hand (deleteByHand)
	// is the app no longer a member of the drive, answered as an id Drive
	// does not have (not measured: only the drive's Manager can remove the
	// app).
	// Unset, rootID is a folder in My Drive that files.get reads, as it does
	// for one the app created.
	sharedDrive bool
	// gone holds the folders deleted by hand, for good: as for any id Drive
	// does not have, files.get answers 404 for each, and a list of its
	// files without corpora 200 with none (measured for a made-up id on the
	// office Pi on 2026-11-11).
	gone map[string]bool
	// trashed holds the folders in the trash. A file in one is in the trash
	// too, as on Drive: a list of what is not trashed leaves it out.
	trashed map[string]bool
	// onUpload, when set, runs before each file upload into a folder, with
	// the folder, while the server holds its lock.
	onUpload func(folder string)
	uploads  int
	// failUploadAt makes precisely one upload request fail before Drive creates
	// its file, so a replacement retry can resume from prior rows.
	failUploadAt int
	// duplicateUploadAt makes precisely one upload create its file twice, as
	// a client retrying a request whose response was lost does; Drive answers
	// with the second copy.
	duplicateUploadAt int
	// allowDeletes permits the replacement-path test to exercise Files.Delete.
	allowDeletes bool
	deletes      int
	failDeleteAt int
	deletedIDs   []string
	// unavailable makes Drive answer 503 to every request for a file.
	unavailable     bool
	corruptChecksum bool
	// driveUnavailable makes Drive answer 503 to every list of a shared
	// drive's files (corpora "drive").
	driveUnavailable bool
	// driveFolderListUnavailable makes Drive answer 503 only to a list of a
	// shared drive's folders (corpora "drive", asking for folders), so the
	// list of any of its files still answers.
	driveFolderListUnavailable bool
	// unscopedListsMiss makes every list without corpora answer 200 with no
	// files, while the shared drive's own list (corpora "drive") still holds
	// the files the app made: Drive not keeping what its documentation says
	// of a list without corpora (the files the user has accessed, shared
	// drives' too), which was not measured with files there.
	unscopedListsMiss bool
	// onGet, when set, runs as Drive is asked for a file (Files.Get), with
	// its id, while the server holds its lock.
	onGet     func(id string)
	gets      int
	failGetAt int
	// onList, when set, runs after Drive lists a folder's files, with the
	// folder, while the server holds its lock: what changes on Drive
	// between the list and the uploads.
	onList func(folder string)
}

// parentQuery reads the parent a list asks for: the query starts with it,
// quoted, with a quote or a backslash in it escaped by a backslash, as Drive
// reads a query.
var parentQuery = regexp.MustCompile(`^'((?:[^'\\]|\\.)*)' in parents`)

// unquote undoes the escapes parentQuery allows.
var unquote = strings.NewReplacer(`\'`, `'`, `\\`, `\`)

// newWireDrive starts the server and returns it with a Syncer whose root
// folder is "root", a folder in My Drive.
func newWireDrive(t *testing.T) (*wireDrive, *Syncer) {
	t.Helper()
	d := &wireDrive{t: t, rootID: "root", clock: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)}
	srv := httptest.NewServer(http.HandlerFunc(d.serve))
	t.Cleanup(srv.Close)
	svc, err := drive.NewService(context.Background(), option.WithEndpoint(srv.URL+"/drive/v3/"), option.WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatalf("drive service: %v", err)
	}
	return d, &Syncer{service: svc, folderID: d.rootID, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// sharedDriveID is the shared drive newSharedDriveWire's Syncer syncs into.
const sharedDriveID = "shared-drive-root"

// newSharedDriveWire is newWireDrive with the Syncer syncing into the root of
// shared drive sharedDriveID, as the drive.file scope sees it (sharedDrive).
func newSharedDriveWire(t *testing.T) (*wireDrive, *Syncer) {
	t.Helper()
	d, s := newWireDrive(t)
	d.rootID, d.sharedDrive, s.folderID = sharedDriveID, true, sharedDriveID
	return d, s
}

// add puts a file in folder parent, as an earlier version of the app left it,
// and returns its id.
func (d *wireDrive) add(parent, name, mimeType string, content []byte, appProperties map[string]string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.create(&wireFile{Name: name, MimeType: mimeType, Parent: parent, AppProperties: appProperties, Content: content}).ID
}

func (d *wireDrive) create(f *wireFile) *wireFile {
	d.next++
	d.clock = d.clock.Add(time.Minute)
	f.ID = fmt.Sprintf("file-%d", d.next)
	f.ModifiedTime = d.clock
	f.RevisionCount++
	d.files = append(d.files, f)
	return f
}

// in returns the files in folder parent, by name.
func (d *wireDrive) in(parent string) map[string][]*wireFile {
	d.mu.Lock()
	defer d.mu.Unlock()
	byName := map[string][]*wireFile{}
	for _, f := range d.files {
		if f.Parent == parent {
			byName[f.Name] = append(byName[f.Name], f)
		}
	}
	return byName
}

func (d *wireDrive) find(id string) *wireFile {
	for _, f := range d.files {
		if f.ID == id {
			return f
		}
	}
	return nil
}

// hasParent reports whether Drive has folder id to create a file in: the
// root, unless deleted by hand, or a folder it holds. A create under any
// other id, a made-up one included, is answered 404 "File not found".
func (d *wireDrive) hasParent(id string) bool {
	return !d.gone[id] && (id == d.rootID || d.find(id) != nil)
}

// deleteByHand deletes folder id and every file in it for good, as a user can
// on Drive (deleting it, then emptying the trash).
func (d *wireDrive) deleteByHand(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.gone == nil {
		d.gone = map[string]bool{}
	}
	d.gone[id] = true
	d.files = slices.DeleteFunc(d.files, func(f *wireFile) bool { return f.ID == id || f.Parent == id })
}

func (d *wireDrive) deleteFileByHand(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.files = slices.DeleteFunc(d.files, func(f *wireFile) bool { return f.ID == id })
}

// trash moves folder id, and every file in it with it, to the trash, as a
// user deleting it in the Drive UI does.
func (d *wireDrive) trash(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.trashLocked(id)
}

// trashLocked is trash for a caller holding the lock (onUpload).
func (d *wireDrive) trashLocked(id string) {
	if d.sharedDrive && id == d.rootID {
		d.t.Errorf("Drive: a shared drive's root (%s) cannot be trashed", id)
	}
	if d.trashed == nil {
		d.trashed = map[string]bool{}
	}
	d.trashed[id] = true
}

// inTrash reports whether file f is in the trash: itself, or a folder above
// it, as Drive reports trashed.
func (d *wireDrive) inTrash(f *wireFile) bool {
	for id, parent := f.ID, f.Parent; ; {
		if d.trashed[id] {
			return true
		}
		if parent == "" {
			return false
		}
		id, parent = parent, ""
		if p := d.find(id); p != nil {
			parent = p.Parent
		}
	}
}

// notFound answers as Drive does for a file it does not have.
func notFound(w http.ResponseWriter, id string) {
	driveNotFound(w, "File not found: "+id+".", "fileId")
}

// sharedDriveNotFound answers as Drive does for a list of a shared drive it
// does not have, and, as the fake takes it (not measured), for one whose
// members the app is not one of.
func sharedDriveNotFound(w http.ResponseWriter, id string) {
	driveNotFound(w, "Shared drive not found: "+id, "driveId")
}

func driveNotFound(w http.ResponseWriter, message, location string) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(http.StatusNotFound)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
		"errors": []map[string]string{{
			"domain": "global", "reason": "notFound", "message": message, "locationType": "parameter", "location": location,
		}},
		"code":    http.StatusNotFound,
		"message": message,
	}})
}

func (d *wireDrive) requireSharedDriveParameters(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasSuffix(r.URL.Path, "/export") && !strings.HasSuffix(r.URL.Path, "/revisions") && r.URL.Query().Get("supportsAllDrives") != "true" {
		d.t.Errorf("Drive: %s %s is missing supportsAllDrives=true", r.Method, r.URL)
		http.Error(w, "supportsAllDrives=true is required", http.StatusBadRequest)
		return false
	}
	if r.Method == http.MethodGet && r.URL.Path == "/drive/v3/files" && r.URL.Query().Get("includeItemsFromAllDrives") != "true" {
		d.t.Errorf("Drive: %s %s is missing includeItemsFromAllDrives=true", r.Method, r.URL)
		http.Error(w, "includeItemsFromAllDrives=true is required", http.StatusBadRequest)
		return false
	}
	return true
}

func (d *wireDrive) serve(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.requireSharedDriveParameters(w, r) {
		return
	}
	path := r.URL.Path
	switch {
	case r.Method == http.MethodDelete:
		id := strings.TrimPrefix(path, "/drive/v3/files/")
		if !d.allowDeletes {
			d.t.Errorf("Drive: %s %s deletes a file", r.Method, path)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		d.deletes++
		if d.failDeleteAt == d.deletes {
			http.Error(w, `{"error":{"code":503,"message":"Backend Error"}}`, http.StatusServiceUnavailable)
			return
		}
		if d.find(id) == nil {
			notFound(w, id)
			return
		}
		d.deletedIDs = append(d.deletedIDs, id)
		d.files = slices.DeleteFunc(d.files, func(f *wireFile) bool { return f.ID == id })
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && path == "/drive/v3/files":
		m := parentQuery.FindStringSubmatch(r.URL.Query().Get("q"))
		if m == nil {
			if !strings.Contains(r.URL.Query().Get("q"), " in parents") {
				d.t.Errorf("Drive: list without a parent: %q", r.URL.Query().Get("q"))
			}
			// As Drive answers a query it cannot read.
			http.Error(w, `{"error":{"code":400,"message":"Invalid Value"}}`, http.StatusBadRequest)
			return
		}
		m[1] = unquote.Replace(m[1])
		if corpora := r.URL.Query().Get("corpora"); corpora != "" {
			driveID := r.URL.Query().Get("driveId")
			if corpora != "drive" || driveID == "" {
				d.t.Errorf("Drive: list with corpora %q and driveId %q", corpora, driveID)
				http.Error(w, "unexpected corpora", http.StatusBadRequest)
				return
			}
			if d.driveUnavailable || (d.driveFolderListUnavailable && strings.Contains(r.URL.Query().Get("q"), "mimeType = 'application/vnd.google-apps.folder'")) {
				http.Error(w, `{"error":{"code":503,"message":"Backend Error"}}`, http.StatusServiceUnavailable)
				return
			}
			if !d.sharedDrive || driveID != d.rootID || d.gone[driveID] {
				sharedDriveNotFound(w, driveID)
				return
			}
		}
		if r.URL.Query().Get("corpora") == "" && d.unscopedListsMiss {
			_ = json.NewEncoder(w).Encode(&drive.FileList{})
			return
		}
		request := wireListRequest{Parent: m[1], PageToken: r.URL.Query().Get("pageToken")}
		d.listRequests = append(d.listRequests, request)
		if pages := d.listPages[request.Parent]; pages != nil {
			page, ok := pages[request.PageToken]
			if !ok {
				d.t.Errorf("Drive: no page for %+v", request)
				http.Error(w, "unknown page token", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(page)
			if d.onList != nil {
				d.onList(request.Parent)
			}
			return
		}
		notTrashed := strings.Contains(r.URL.Query().Get("q"), "trashed = false")
		list := &drive.FileList{}
		for _, f := range d.files {
			if f.Parent == m[1] && (!notTrashed || !d.inTrash(f)) {
				file := &drive.File{Id: f.ID, Name: f.Name, MimeType: f.MimeType,
					AppProperties: f.AppProperties, ModifiedTime: f.ModifiedTime.Format(time.RFC3339Nano)}
				if strings.Contains(r.URL.Query().Get("fields"), "md5Checksum") {
					sum := md5.Sum(f.Content)
					file.Md5Checksum = fmt.Sprintf("%x", sum)
				}
				list.Files = append(list.Files, file)
			}
		}
		_ = json.NewEncoder(w).Encode(list)
		if d.onList != nil {
			d.onList(m[1])
		}
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/revisions"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/drive/v3/files/"), "/revisions")
		f := d.find(id)
		if f == nil {
			notFound(w, id)
			return
		}
		revisions := make([]*drive.Revision, 0, f.RevisionCount)
		for i := range f.RevisionCount {
			revisions = append(revisions, &drive.Revision{Id: strconv.Itoa(i + 1)})
		}
		_ = json.NewEncoder(w).Encode(&drive.RevisionList{Revisions: revisions})
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/export"):
		f := d.find(strings.TrimSuffix(strings.TrimPrefix(path, "/drive/v3/files/"), "/export"))
		if f == nil {
			http.Error(w, "no such file", http.StatusNotFound)
			return
		}
		_, _ = w.Write(f.Content)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/drive/v3/files/"):
		id := strings.TrimPrefix(path, "/drive/v3/files/")
		if d.onGet != nil {
			d.onGet(id)
		}
		d.gets++
		if d.failGetAt == d.gets || d.unavailable {
			http.Error(w, `{"error":{"code":503,"message":"Backend Error"}}`, http.StatusServiceUnavailable)
			return
		}
		if id == d.rootID && !d.gone[id] && !d.sharedDrive {
			// The folder the Syncer syncs into, a folder in My Drive. A
			// shared drive's root is not found, as for the drive.file scope.
			_ = json.NewEncoder(w).Encode(&drive.File{Id: id, MimeType: "application/vnd.google-apps.folder", Trashed: d.trashed[id], ExplicitlyTrashed: d.trashed[id]})
			return
		}
		f := d.find(id)
		if f == nil {
			notFound(w, id)
			return
		}
		if r.URL.Query().Get("alt") == "media" {
			_, _ = w.Write(f.Content)
			return
		}
		sum := sha256.Sum256(f.Content)
		checksum := hex.EncodeToString(sum[:])
		if d.corruptChecksum {
			checksum = "corrupt"
		}
		_ = json.NewEncoder(w).Encode(&drive.File{Id: f.ID, Name: f.Name, MimeType: f.MimeType, AppProperties: f.AppProperties, Sha256Checksum: checksum, Size: int64(len(f.Content)), Trashed: d.inTrash(f), ExplicitlyTrashed: d.trashed[f.ID]})
	case r.Method == http.MethodPatch && strings.HasPrefix(path, "/drive/v3/files/"):
		f := d.find(strings.TrimPrefix(path, "/drive/v3/files/"))
		if f == nil {
			notFound(w, strings.TrimPrefix(path, "/drive/v3/files/"))
			return
		}
		var meta drive.File
		if err := json.NewDecoder(r.Body).Decode(&meta); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if meta.Trashed {
			d.t.Errorf("Drive: %s %s trashes a file", r.Method, path)
		}
		if meta.Name != "" {
			f.Name = meta.Name
		}
		d.clock = d.clock.Add(time.Minute)
		f.ModifiedTime = d.clock
		_ = json.NewEncoder(w).Encode(&drive.File{Id: f.ID})
	case r.Method == http.MethodPost && path == "/drive/v3/files":
		var meta drive.File
		if err := json.NewDecoder(r.Body).Decode(&meta); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if parent := firstParent(meta.Parents); !d.hasParent(parent) {
			notFound(w, parent)
			return
		}
		f := d.create(&wireFile{Name: meta.Name, MimeType: meta.MimeType, Parent: firstParent(meta.Parents)})
		_ = json.NewEncoder(w).Encode(&drive.File{Id: f.ID})
	case r.Method == http.MethodPost && path == "/upload/drive/v3/files":
		meta, content := d.multipart(w, r)
		if meta == nil {
			return
		}
		parent := firstParent(meta.Parents)
		if !d.hasParent(parent) {
			notFound(w, parent)
			return
		}
		d.uploads++
		if d.failUploadAt == d.uploads {
			http.Error(w, `{"error":{"code":503,"message":"Backend Error"}}`, http.StatusServiceUnavailable)
			return
		}
		if d.onUpload != nil {
			d.onUpload(parent)
		}
		f := d.create(&wireFile{Name: meta.Name, MimeType: meta.MimeType, Parent: firstParent(meta.Parents), AppProperties: meta.AppProperties, Content: content})
		if d.duplicateUploadAt == d.uploads {
			f = d.create(&wireFile{Name: meta.Name, MimeType: meta.MimeType, Parent: firstParent(meta.Parents), AppProperties: maps.Clone(meta.AppProperties), Content: content})
		}
		_ = json.NewEncoder(w).Encode(&drive.File{Id: f.ID})
	case r.Method == http.MethodPatch && strings.HasPrefix(path, "/upload/drive/v3/files/"):
		f := d.find(strings.TrimPrefix(path, "/upload/drive/v3/files/"))
		if f == nil {
			http.Error(w, "no such file", http.StatusNotFound)
			return
		}
		if d.onUpload != nil {
			d.onUpload(f.Parent)
		}
		meta, content := d.multipart(w, r)
		if meta == nil {
			return
		}
		if meta.Trashed {
			d.t.Errorf("Drive: %s %s trashes a file", r.Method, path)
		}
		f.Content = content
		f.RevisionCount++
		for k, v := range meta.AppProperties {
			if f.AppProperties == nil {
				f.AppProperties = map[string]string{}
			}
			f.AppProperties[k] = v
		}
		d.clock = d.clock.Add(time.Minute)
		f.ModifiedTime = d.clock
		_ = json.NewEncoder(w).Encode(&drive.File{Id: f.ID})
	default:
		d.t.Errorf("Drive: unexpected request %s %s", r.Method, r.URL)
		http.Error(w, "unexpected", http.StatusNotImplemented)
	}
}

// multipart reads a multipart upload: its metadata and its content.
func (d *wireDrive) multipart(w http.ResponseWriter, r *http.Request) (*drive.File, []byte) {
	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		d.t.Errorf("Drive: upload content type %q: %v", r.Header.Get("Content-Type"), err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return nil, nil
	}
	parts := multipart.NewReader(r.Body, params["boundary"])
	metaPart, err := parts.NextPart()
	if err != nil {
		d.t.Errorf("Drive: upload metadata: %v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return nil, nil
	}
	var meta drive.File
	if err := json.NewDecoder(metaPart).Decode(&meta); err != nil {
		d.t.Errorf("Drive: upload metadata: %v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return nil, nil
	}
	mediaPart, err := parts.NextPart()
	if err != nil {
		d.t.Errorf("Drive: upload media: %v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return nil, nil
	}
	content, err := io.ReadAll(mediaPart)
	if err != nil {
		d.t.Errorf("Drive: upload media: %v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return nil, nil
	}
	return &meta, content
}

func firstParent(parents []string) string {
	if len(parents) == 0 {
		return ""
	}
	return parents[0]
}
