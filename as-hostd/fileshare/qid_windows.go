//go:build windows

package fileshare

import (
	"os"
	"sync"
	"sync/atomic"
)

// On Windows, os.FileInfo doesn't expose a stable file ID directly. A
// robust implementation would call GetFileInformationByHandleEx with
// FILE_ID_INFO to get (VolumeSerialNumber, FileId). For v1 we use a
// per-path synthetic ID: a monotonically assigned uint64 keyed by the
// normalized path. Collisions only occur on rename-after-unlink of a
// different path, which would at worst cause a kernel 9p-client cache
// miss — no correctness bug.
var (
	idMu  sync.Mutex
	idMap = make(map[string]uint64)
	idSeq atomic.Uint64
)

func qidPath(hostPath string, fi os.FileInfo) uint64 {
	idMu.Lock()
	defer idMu.Unlock()
	if v, ok := idMap[hostPath]; ok {
		return v
	}
	v := idSeq.Add(1)
	idMap[hostPath] = v
	return v
}
