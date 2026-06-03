//go:build linux

package runner

import "syscall"

// StatfsBlockBytes returns the byte size of a single block of the filesystem
// described by stat. On Linux, `Statfs_t.Blocks` is measured in `Frsize` units
// (fragment size), not `Bsize` (preferred I/O transfer size). Bsize is purely
// advisory and on overlay/fuse filesystems (used by RunPod and Vast container
// roots) can be much larger than Frsize — causing `Blocks * Bsize` to over-
// report total disk capacity by orders of magnitude.
//
// Falls back to Bsize if Frsize is unreported (Frsize == 0), which can happen
// on older kernels or unusual filesystems.
func StatfsBlockBytes(stat syscall.Statfs_t) int64 {
	if stat.Frsize > 0 {
		return int64(stat.Frsize)
	}
	return int64(stat.Bsize)
}
