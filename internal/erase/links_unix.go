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

// noFollow makes an open of a symbolic link fail rather than open the file
// it points to.
const noFollow = syscall.O_NOFOLLOW
