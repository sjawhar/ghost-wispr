//go:build !unix

package storage

import "os"

// fileSystemOf returns 0: this system does not report a file's device, so an
// erase directory counts as cleaned whenever it is there.
func fileSystemOf(os.FileInfo) int64 { return 0 }
