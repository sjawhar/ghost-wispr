package gdrive

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"regexp"
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

// wireDrive is the part of the Google Drive v3 HTTP API the Syncer uses,
// served over httptest, so the real Syncer (its requests on the wire) runs
// against it. It fails the test on any request that deletes or trashes a
// file, or that it does not know.
type wireDrive struct {
	t     *testing.T
	mu    sync.Mutex
	files []*wireFile // in listing order
	next  int
	clock time.Time
}

var parentQuery = regexp.MustCompile(`'([^']+)' in parents`)

// newWireDrive starts the server and returns it with a Syncer whose root
// folder is "root".
func newWireDrive(t *testing.T) (*wireDrive, *Syncer) {
	t.Helper()
	d := &wireDrive{t: t, clock: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)}
	srv := httptest.NewServer(http.HandlerFunc(d.serve))
	t.Cleanup(srv.Close)
	svc, err := drive.NewService(context.Background(), option.WithEndpoint(srv.URL+"/drive/v3/"), option.WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatalf("drive service: %v", err)
	}
	return d, &Syncer{service: svc, folderID: "root", logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
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

func (d *wireDrive) serve(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
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
		list := &drive.FileList{}
		for _, f := range d.files {
			if f.Parent == m[1] {
				list.Files = append(list.Files, &drive.File{Id: f.ID, Name: f.Name, MimeType: f.MimeType,
					AppProperties: f.AppProperties, ModifiedTime: f.ModifiedTime.Format(time.RFC3339Nano)})
			}
		}
		_ = json.NewEncoder(w).Encode(list)
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/export"):
		f := d.find(strings.TrimSuffix(strings.TrimPrefix(path, "/drive/v3/files/"), "/export"))
		if f == nil {
			http.Error(w, "no such file", http.StatusNotFound)
			return
		}
		_, _ = w.Write(f.Content)
	case r.Method == http.MethodPost && path == "/drive/v3/files":
		var meta drive.File
		if err := json.NewDecoder(r.Body).Decode(&meta); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f := d.create(&wireFile{Name: meta.Name, MimeType: meta.MimeType, Parent: firstParent(meta.Parents)})
		_ = json.NewEncoder(w).Encode(&drive.File{Id: f.ID})
	case r.Method == http.MethodPost && path == "/upload/drive/v3/files":
		meta, content := d.multipart(w, r)
		if meta == nil {
			return
		}
		f := d.create(&wireFile{Name: meta.Name, MimeType: meta.MimeType, Parent: firstParent(meta.Parents), AppProperties: meta.AppProperties, Content: content})
		_ = json.NewEncoder(w).Encode(&drive.File{Id: f.ID})
	case r.Method == http.MethodPatch && strings.HasPrefix(path, "/upload/drive/v3/files/"):
		f := d.find(strings.TrimPrefix(path, "/upload/drive/v3/files/"))
		if f == nil {
			http.Error(w, "no such file", http.StatusNotFound)
			return
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
