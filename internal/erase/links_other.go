//go:build !unix

package erase

import "os"

// links returns 1: this system does not report a file's link count.
func links(os.FileInfo) uint64 { return 1 }

// noFollow is 0: this system has no flag to refuse a symbolic link at open;
// finish skips anything not a regular file.
const noFollow = 0
