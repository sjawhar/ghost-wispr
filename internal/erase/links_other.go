//go:build !unix

package erase

import "os"

// links returns 1: this system does not report a file's link count. So here
// a second name that Replace keeps for a file it has not replaced yet reads
// as the only one: Leftovers and Orphans would overwrite the file it still
// names, and eraseFile overwrites a file another name still links. The
// release builds only for Linux (.github/workflows/release.yml), where links
// reads the count.
func links(os.FileInfo) uint64 { return 1 }

// noFollow is 0: this system has no flag to refuse a symbolic link at open.
// finish skips anything not a regular file, so Leftovers and Orphans do not
// follow one; a link planted at a hidden name between that check and the
// open would be followed.
const noFollow = 0
