// Package secrets reads secret versions from Google Secret Manager.
package secrets

import (
	"context"
	"encoding/base64"
	"fmt"
	"hash/crc32"

	"google.golang.org/api/option"
	"google.golang.org/api/secretmanager/v1"
)

// Reader reads secret versions from Secret Manager.
type Reader struct {
	versions *secretmanager.ProjectsSecretsVersionsService
}

// NewReader returns a Reader that calls Secret Manager with opts, such as
// option.WithAuthCredentials for the unit's Google credential.
func NewReader(ctx context.Context, opts ...option.ClientOption) (*Reader, error) {
	service, err := secretmanager.NewService(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("create Secret Manager client: %w", err)
	}
	return &Reader{versions: service.Projects.Secrets.Versions}, nil
}

var castagnoliTable = crc32.MakeTable(crc32.Castagnoli)

// Read returns the payload of the secret version name names
// (projects/<project>/secrets/<secret>/versions/<version>), checked against
// the CRC32C Secret Manager sends with it.
func (r *Reader) Read(ctx context.Context, name string) (string, error) {
	response, err := r.versions.Access(name).Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("access secret %s: %w", name, err)
	}
	if response.Payload == nil {
		return "", fmt.Errorf("secret %s has no payload", name)
	}
	data, err := base64.StdEncoding.DecodeString(response.Payload.Data)
	if err != nil {
		return "", fmt.Errorf("decode secret %s: %w", name, err)
	}
	if response.Payload.DataCrc32c != 0 && int64(crc32.Checksum(data, castagnoliTable)) != response.Payload.DataCrc32c {
		return "", fmt.Errorf("secret %s arrived corrupted: its CRC32C does not match", name)
	}
	return string(data), nil
}
