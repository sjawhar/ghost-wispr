package gdrive

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"

	"github.com/sjawhar/ghost-wispr/internal/logging"
)

// GOOGLE_SDK_GO_LOGGING_LEVEL=debug makes the Drive SDK write each request
// whole to stderr, the journal, around the app's logger: a session folder's
// name, which holds its title. logging.WithholdGoogleSDKLogging turns it off.
// The control shows the SDK writes the planted word when the variable stays.
func TestGoogleSDKLoggingIsWithheld(t *testing.T) {
	const planted = "quokkaplantedroomword"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"folder-1"}`)
	}))
	t.Cleanup(server.Close)
	createFolder := func() string {
		t.Helper()
		read, write, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		stderr := os.Stderr
		os.Stderr = write
		svc, err := drive.NewService(context.Background(), option.WithEndpoint(server.URL+"/drive/v3/"), option.WithHTTPClient(server.Client()))
		if err == nil {
			_, err = svc.Files.Create(&drive.File{Name: "2026-11-20 " + planted, MimeType: "application/vnd.google-apps.folder"}).Do()
		}
		os.Stderr = stderr
		_ = write.Close()
		logged, _ := io.ReadAll(read)
		if err != nil {
			t.Fatal(err)
		}
		return string(logged)
	}

	t.Setenv("GOOGLE_SDK_GO_LOGGING_LEVEL", "debug")
	if logged := createFolder(); !strings.Contains(logged, planted) {
		t.Fatalf("control: with GOOGLE_SDK_GO_LOGGING_LEVEL=debug the Drive SDK wrote no request to stderr, so the check reads nothing:\n%s", logged)
	}
	if !logging.WithholdGoogleSDKLogging() {
		t.Fatal("WithholdGoogleSDKLogging did not see the variable on")
	}
	if logged := createFolder(); logged != "" {
		t.Fatalf("the Drive SDK still wrote to stderr:\n%s", logged)
	}
	if logging.WithholdGoogleSDKLogging() {
		t.Error("WithholdGoogleSDKLogging saw the variable on again")
	}
}
