package gdrive

import (
	"context"
	"errors"

	"google.golang.org/api/drive/v3"
)

// syncOnlyDrive is the part of a test Drive that the tests using it do not
// exercise: it keeps no folder names (RenameFolder does nothing) and
// replaces no files (each replacement step fails), so a test that reaches a
// replacement fails.
type syncOnlyDrive struct{}

var errTestDriveReplaces = errors.New("this test Drive does not replace files")

func (syncOnlyDrive) RenameFolder(context.Context, string, string) error { return nil }

func (syncOnlyDrive) Serialize(fn func() error) error { return fn() }

func (syncOnlyDrive) CheckFolder(context.Context, string) error { return errTestDriveReplaces }

func (syncOnlyDrive) ListFolderFiles(context.Context, string) ([]*drive.File, error) {
	return nil, errTestDriveReplaces
}

func (syncOnlyDrive) UploadFile(context.Context, string, *SyncFile) (string, error) {
	return "", errTestDriveReplaces
}

func (syncOnlyDrive) VerifyFile(context.Context, string, *SyncFile) error {
	return errTestDriveReplaces
}

func (syncOnlyDrive) DeleteFile(context.Context, string) error { return errTestDriveReplaces }
