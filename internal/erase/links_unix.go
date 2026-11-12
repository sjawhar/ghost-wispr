//go:build unix

package erase

import (
	"os"
	"syscall"
)

// links returns how many names link the file info describes.
func links(info os.FileInfo) uint64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Nlink)
	}
	return 1
}
