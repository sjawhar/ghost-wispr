package gdrive

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
)

// TestADriveErrorDoesNotQuoteTheRequest: Drive can quote in its error the
// request it was sent: a session folder's name holds the session's title, and
// a document's content is its transcript or summary. Every Drive call's error
// is logged and kept as the session's sync error, so it keeps Drive's status
// and reason and never Drive's message. Drive answers each call 400, with a
// message and a body that quote the whole request.
func TestADriveErrorDoesNotQuoteTheRequest(t *testing.T) {
	const planted = "quokkaplantedroomword"
	quoted := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request, _ := io.ReadAll(r.Body)
		echo := r.URL.String() + " " + string(request)
		if strings.Contains(echo, planted) {
			quoted++
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintf(w, `{"error":{"code":400,"message":%q,"errors":[{"reason":"badRequest","message":%q}]}}`, "Invalid value: "+echo, echo)
	}))
	t.Cleanup(server.Close)
	svc, err := drive.NewService(context.Background(), option.WithEndpoint(server.URL+"/drive/v3/"), option.WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	s := &Syncer{service: svc, folderID: "root-" + planted, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx := context.Background()
	recording := filepath.Join(t.TempDir(), "recording.mp3")
	if err := os.WriteFile(recording, []byte("audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	document := SyncFile{Name: "transcript.md", MimeType: "text/markdown", ContentType: "text/markdown", Content: []byte("We agreed on " + planted + ".")}

	calls := map[string]func() error{
		"Upload": func() error {
			_, err := s.Upload(ctx, "2026-11-20 "+planted, []SyncFile{document})
			return err
		},
		"UploadFile from a path": func() error {
			_, err := s.UploadFile(ctx, "folder", &SyncFile{Name: "recording.mp3", MimeType: "audio/mpeg", ContentType: "audio/mpeg", LocalPath: recording})
			return err
		},
		"CheckFolder": func() error { return s.CheckFolder(ctx, "folder-"+planted) },
		"RecordingsMatch": func() error {
			_, err := s.RecordingsMatch(ctx, "folder-"+planted, map[string]string{"a.mp3": "x"})
			return err
		},
		"UploadFile":         func() error { _, err := s.UploadFile(ctx, "folder", &document); return err },
		"updateDocument":     func() error { return s.updateDocument(ctx, &docUpdate{ID: "doc", File: document}) },
		"DeleteFile":         func() error { return s.DeleteFile(ctx, "file-"+planted) },
		"RenameFolder":       func() error { return s.RenameFolder(ctx, "folder", "2026-11-20 "+planted) },
		"ListFolderFiles":    func() error { _, err := s.ListFolderFiles(ctx, "folder-"+planted); return err },
		"ListSessionFolders": func() error { _, err := s.ListSessionFolders(ctx); return err },
		"VerifyFile":         func() error { return s.VerifyFile(ctx, "file", &document) },
		"ExportGoogleDoc":    func() error { _, err := s.ExportGoogleDoc(ctx, "doc-"+planted); return err },
		"DownloadRecording": func() error {
			return s.DownloadRecording(ctx, "file-"+planted, "recording-"+planted+".mp3", filepath.Join(t.TempDir(), "restored.mp3"))
		},
	}
	for name, call := range calls {
		err := call()
		if err == nil {
			t.Errorf("%s: no error from a Drive that answers 400", name)
			continue
		}
		// Ghost Wispr's own text names Drive ids; only Drive's echo holds
		// these: its message, a folder's name, a document's content.
		for _, quote := range []string{"Invalid value", "2026-11-20", "We agreed on"} {
			if strings.Contains(err.Error(), quote) {
				t.Errorf("%s: the error quotes Drive's message (%q): %v", name, quote, err)
			}
		}
		if !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "badRequest") {
			t.Errorf("%s: the error %q does not name status 400 and reason badRequest", name, err)
		}
	}
	if quoted == 0 {
		t.Fatal("no request carried the planted word, so Drive's answers quoted nothing")
	}
}

// A file Drive returns whose hash or checksum differs from the one uploaded
// fails VerifyFile without quoting what Drive returned: Drive's fields are
// its text, and the error is logged and shown on GET /api/status.
func TestAVerifyFailureDoesNotQuoteDrivesFields(t *testing.T) {
	const planted = "quokkaplantedroomword"
	recording := filepath.Join(t.TempDir(), "recording.mp3")
	if err := os.WriteFile(recording, []byte("audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, answer := range []string{
		`{"id":"file","appProperties":{"ghostWisprSHA256":"` + planted + `"}}`,
		`{"id":"file","appProperties":{"ghostWisprSHA256":"expected"},"sha256Checksum":"` + planted + `","size":"5"}`,
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, answer)
		}))
		svc, err := drive.NewService(context.Background(), option.WithEndpoint(server.URL+"/drive/v3/"), option.WithHTTPClient(server.Client()))
		if err != nil {
			t.Fatal(err)
		}
		s := &Syncer{service: svc, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
		err = s.VerifyFile(context.Background(), "file", &SyncFile{Name: "recording.mp3", SHA256: "expected", LocalPath: recording})
		server.Close()
		if err == nil || strings.Contains(err.Error(), planted) {
			t.Errorf("VerifyFile = %v, want a failure that quotes nothing Drive returned, for %s", err, answer)
		}
	}
}
