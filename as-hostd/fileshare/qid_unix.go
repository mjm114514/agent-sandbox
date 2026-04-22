//go:build !windows

package fileshare

import (
	"os"
	"syscall"
)

// On Linux/macOS, the inode number is stable per (dev, ino). We fold both
// into a single uint64 — high bits of dev, low bits of ino — which is
// sufficient as a QID path since the kernel 9p client treats it as opaque.
func qidPath(hostPath string, fi os.FileInfo) uint64 {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return uint64(st.Ino) ^ (uint64(st.Dev) << 48)
}
