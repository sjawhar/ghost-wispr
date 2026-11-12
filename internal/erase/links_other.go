//go:build !unix

package erase

import "os"

// links returns 1: this system does not report a file's link count.
func links(os.FileInfo) uint64 { return 1 }
