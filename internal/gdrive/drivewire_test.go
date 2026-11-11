package gdrive

import (
	"context"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
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
	// gone holds the folders deleted by hand, for good: Drive answers 404
	// for each, as for any file it does not have.
	gone map[string]bool
	// trashed holds the folders in the trash. A file in one is in the trash
	// too, as on Drive: a list of what is not trashed leaves it out.
	trashed map[string]bool
	// onUpload, when set, runs before each file upload into a folder, with
	// the folder, while the server holds its lock.
	onUpload func(folder string)
	// unavailable makes Drive answer 503 to every request for a file.
	unavailable bool
	// onGet, when set, runs as Drive is asked for a file (Files.Get), with
	// its id, while the server holds its lock.
	onGet func(id string)
	// onList, when set, runs after Drive lists a folder's files, with the
	// folder, while the server holds its lock: what changes on Drive
	// between the list and the uploads.
	onList func(folder string)
}

var parentQuery = regexp.MustCompile(`'([^']+)' in parents`)

// newWireDrive starts the server and returns it with a Syncer whose root
// folder is "root".
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

// trash moves folder id, and every file in it with it, to the trash, as a
// user deleting it in the Drive UI does.
func (d *wireDrive) trash(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.trashLocked(id)
}

// trashLocked is trash for a caller holding the lock (onUpload).
func (d *wireDrive) trashLocked(id string) {
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
	message := "File not found: " + id + "."
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(http.StatusNotFound)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
		"errors": []map[string]string{{
			"domain": "global", "reason": "notFound", "message": message, "locationType": "parameter", "location": "fileId",
		}},
		"code":    http.StatusNotFound,
		"message": message,
	}})
}

func (d *wireDrive) requireSharedDriveParameters(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasSuffix(r.URL.Path, "/export") && r.URL.Query().Get("supportsAllDrives") != "true" {
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
		d.t.Errorf("Drive: %s %s deletes a file", r.Method, path)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && path == "/drive/v3/files":
		m := parentQuery.FindStringSubmatch(r.URL.Query().Get("q"))
		if m == nil {
			d.t.Errorf("Drive: list without a parent: %q", r.URL.Query().Get("q"))
			http.Error(w, "bad query", http.StatusBadRequest)
			return
		}
		if d.gone[m[1]] {
			notFound(w, m[1])
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
		if d.unavailable {
			http.Error(w, `{"error":{"code":503,"message":"Backend Error"}}`, http.StatusServiceUnavailable)
			return
		}
		if id == d.rootID && !d.gone[id] {
			// The folder the Syncer syncs into. It is a shared-drive root,
			// whose ID is also its Drive ID.
			_ = json.NewEncoder(w).Encode(&drive.File{Id: id, DriveId: d.rootID, Trashed: d.trashed[id], ExplicitlyTrashed: d.trashed[id]})
			return
		}
		f := d.find(id)
		if f == nil {
			notFound(w, id)
			return
		}
		_ = json.NewEncoder(w).Encode(&drive.File{Id: f.ID, Name: f.Name, MimeType: f.MimeType, Trashed: d.inTrash(f), ExplicitlyTrashed: d.trashed[f.ID]})
	case r.Method == http.MethodPost && path == "/drive/v3/files":
		var meta drive.File
		if err := json.NewDecoder(r.Body).Decode(&meta); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if parent := firstParent(meta.Parents); d.gone[parent] {
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
		if d.gone[parent] {
			notFound(w, parent)
			return
		}
		if d.onUpload != nil {
			d.onUpload(parent)
		}
		f := d.create(&wireFile{Name: meta.Name, MimeType: meta.MimeType, Parent: firstParent(meta.Parents), AppProperties: meta.AppProperties, Content: content})
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
