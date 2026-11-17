package secrets

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"hash/crc32"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/api/option"
)

const testSecretName = "projects/example-project/secrets/example-signing-secret/versions/latest"

// fakeSecretManager answers Secret Manager's access call for one version
// name with payload, and counts the calls.
func fakeSecretManager(t *testing.T, name, payload string, crc int64) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/v1/"+name+":access" {
			http.Error(w, `{"error":{"code":404,"message":"not found"}}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name": strings.TrimSuffix(name, "latest") + "3",
			"payload": map[string]any{
				"data":       base64.StdEncoding.EncodeToString([]byte(payload)),
				"dataCrc32c": strconv.FormatInt(crc, 10),
			},
		})
	}))
	t.Cleanup(server.Close)
	return server, &calls
}

func testReader(t *testing.T, server *httptest.Server) *Reader {
	t.Helper()
	reader, err := NewReader(context.Background(), option.WithEndpoint(server.URL), option.WithoutAuthentication(), option.WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	return reader
}

func castagnoli(data string) int64 {
	return int64(crc32.Checksum([]byte(data), crc32.MakeTable(crc32.Castagnoli)))
}

// TestReadReturnsTheSecretVersionsPayload fails if Read does not ask Secret
// Manager for the version it names and return its payload.
func TestReadReturnsTheSecretVersionsPayload(t *testing.T) {
	server, calls := fakeSecretManager(t, testSecretName, "signing-key\n", castagnoli("signing-key\n"))
	got, err := testReader(t, server).Read(context.Background(), testSecretName)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got != "signing-key\n" {
		t.Fatalf("Read = %q, want the payload", got)
	}
	if calls.Load() != 1 {
		t.Fatalf("Secret Manager calls = %d, want 1", calls.Load())
	}
}

// TestReadRefusesACorruptedPayload fails if Read returns a payload whose
// CRC32C is not the one Secret Manager sent with it.
func TestReadRefusesACorruptedPayload(t *testing.T) {
	server, _ := fakeSecretManager(t, testSecretName, "signing-key", castagnoli("another key"))
	if got, err := testReader(t, server).Read(context.Background(), testSecretName); err == nil {
		t.Fatalf("Read of a payload with the wrong CRC32C = %q, want an error", got)
	}
}

// TestReadReportsAMissingSecret fails if Read hides Secret Manager's refusal.
func TestReadReportsAMissingSecret(t *testing.T) {
	server, _ := fakeSecretManager(t, testSecretName, "signing-key", castagnoli("signing-key"))
	_, err := testReader(t, server).Read(context.Background(), "projects/p/secrets/other/versions/latest")
	if err == nil || !strings.Contains(err.Error(), "projects/p/secrets/other/versions/latest") {
		t.Fatalf("Read of a missing secret: err = %v, want an error naming it", err)
	}
}
