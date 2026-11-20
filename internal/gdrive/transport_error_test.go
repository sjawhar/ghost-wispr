package gdrive

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
)

// driveBodyBreaks lists a folder holding recording.mp3 and answers every
// download and export with a body whose read fails with err.
type driveBodyBreaks struct{ err error }

func (d driveBodyBreaks) RoundTrip(r *http.Request) (*http.Response, error) {
	body := io.NopCloser(errReader(d))
	if r.URL.Query().Get("alt") != "media" && !strings.HasSuffix(r.URL.Path, "/export") {
		body = io.NopCloser(strings.NewReader(`{"files":[{"id":"rec-1","name":"recording.mp3","mimeType":"audio/mpeg"}]}`))
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: body, Request: r}, nil
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// A recording download or a document export whose answer cannot be read
// fails without quoting what Drive sent: net/http's own errors quote an
// answer it could not read, as an HTTP/2 GOAWAY's debug text does.
func TestADriveBodyReadFailureQuotesNothing(t *testing.T) {
	const planted = "quokkaplantedroomword"
	goaway := errors.New(`http2: server sent GOAWAY and closed the connection; debug="` + planted + `"`)
	svc, err := drive.NewService(context.Background(), option.WithEndpoint("https://drive.example/drive/v3/"), option.WithHTTPClient(&http.Client{Transport: driveBodyBreaks{goaway}}))
	if err != nil {
		t.Fatal(err)
	}
	s := &Syncer{service: svc, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx := context.Background()

	downloadErr := s.DownloadRecording(ctx, "folder", "recording.mp3", filepath.Join(t.TempDir(), "recording.mp3"))
	_, exportErr := s.ExportGoogleDoc(ctx, "doc")
	for name, err := range map[string]error{"DownloadRecording": downloadErr, "ExportGoogleDoc": exportErr} {
		if err == nil || strings.Contains(err.Error(), planted) || !strings.Contains(err.Error(), "withheld") {
			t.Errorf("%s = %v, want the body's read failure withheld", name, err)
		}
	}
}
