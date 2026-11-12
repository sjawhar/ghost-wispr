//go:build unix

package storage

import (
	"os"
	"syscall"
)

// fileSystemOf returns the id of the device the file info describes is on:
// a directory below a mount point that is not mounted reads the device of
// the file system beneath.
func fileSystemOf(info os.FileInfo) int64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return int64(st.Dev) //nolint:unconvert // Dev is int32 on some systems.
	}
	return 0
}
