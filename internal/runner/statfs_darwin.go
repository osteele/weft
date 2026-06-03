//go:build darwin

package runner

import "syscall"

// StatfsBlockBytes returns the byte size of a single block of the filesystem
// described by stat. Darwin's BSD-style statfs does not expose a separate
// fragment size; Blocks is in units of Bsize, so Blocks*Bsize is the correct
// total byte count.
func StatfsBlockBytes(stat syscall.Statfs_t) int64 {
	return int64(stat.Bsize)
}
