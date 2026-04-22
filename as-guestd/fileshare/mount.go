// Package fileshare handles the guest side of the File Guard 9P mount.
//
// On a file_share.mount RPC, we dial the host's 9P server over vsock, hand
// the fd to mount(8) via -o trans=fd, and let the kernel 9P client take
// ownership. See docs/file-guard.md for the architecture.
package fileshare

import (
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

// AF_VSOCK and the sockaddr struct aren't exported by x/sys/unix on all
// go mod versions we build against, so we do the syscall ourselves.
const afVsock = 40

type sockaddrVM struct {
	Family   uint16
	Reserved uint16
	Port     uint32
	CID      uint32
	Zero     [4]uint8
}

type MountParams struct {
	EnvName   string `json:"env_name"`
	GuestPath string `json:"guest_path"`
	VsockPort uint32 `json:"vsock_port"`
}

type Mounter struct {
	hostCID uint32
	mu      sync.Mutex
	active  map[string]struct{} // guest_path set
}

func New(hostCID uint32) *Mounter {
	return &Mounter{hostCID: hostCID, active: make(map[string]struct{})}
}

// Mount dials the host 9P server, writes the aname frame, then mounts the
// resulting fd at guest_path via mount -t 9p -o trans=fd,...
func (m *Mounter) Mount(p MountParams) error {
	m.mu.Lock()
	if _, ok := m.active[p.GuestPath]; ok {
		m.mu.Unlock()
		return fmt.Errorf("already mounted at %s", p.GuestPath)
	}
	m.mu.Unlock()

	if err := os.MkdirAll(p.GuestPath, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", p.GuestPath, err)
	}

	port := p.VsockPort
	if port == 0 {
		port = 1002
	}

	// Open a raw AF_VSOCK socket ourselves (not through net/netpoll).
	// Keeps the fd simple, blocking, and entirely outside Go's runtime
	// netpoller — the kernel 9P client takes over via mount(2).
	fdInt, err := syscall.Socket(afVsock, syscall.SOCK_STREAM, 0)
	if err != nil {
		return fmt.Errorf("socket AF_VSOCK: %w", err)
	}
	success := false
	defer func() {
		if !success {
			syscall.Close(fdInt)
		}
	}()

	sa := sockaddrVM{Family: afVsock, Port: port, CID: m.hostCID}
	_, _, errno := syscall.Syscall(syscall.SYS_CONNECT, uintptr(fdInt),
		uintptr(unsafe.Pointer(&sa)), unsafe.Sizeof(sa))
	if errno != 0 {
		return fmt.Errorf("vsock connect cid=%d port=%d: %w", m.hostCID, port, errno)
	}

	aname := p.EnvName + "|" + p.GuestPath
	hdr := []byte{byte(len(aname) >> 8), byte(len(aname))}
	if _, err := syscall.Write(fdInt, hdr); err != nil {
		return fmt.Errorf("write aname header: %w", err)
	}
	if _, err := syscall.Write(fdInt, []byte(aname)); err != nil {
		return fmt.Errorf("write aname: %w", err)
	}

	// Best-effort: load the 9p and fd transport modules in case
	// /etc/modules processing didn't.
	_ = exec.Command("modprobe", "9p").Run()
	_ = exec.Command("modprobe", "9pnet_fd").Run()

	// Call mount(2) directly from this process. fdInt is in our fd table
	// and the kernel picks it up via rfdno/wfdno.
	fdNum := uintptr(fdInt)
	// aname in the mount options goes into the 9P Tattach message; our
	// server routes via the wire frame written above, not via Tattach.
	// So we pass aname=/ (attach-at-root) rather than the routing key.
	opts := fmt.Sprintf(
		"trans=fd,rfdno=%d,wfdno=%d,uname=root,aname=/,version=9p2000.L,msize=65536",
		fdNum, fdNum,
	)
	mountErr := syscall.Mount("none", p.GuestPath, "9p", 0, opts)
	if mountErr != nil {
		return fmt.Errorf("mount 9p at %s: %w\n  opts: %s", p.GuestPath, mountErr, opts)
	}
	// Kernel fget'd the fd; we can close our ref. (The defer above
	// would also have done this on failure.)
	success = true
	syscall.Close(fdInt)

	m.mu.Lock()
	m.active[p.GuestPath] = struct{}{}
	m.mu.Unlock()
	return nil
}

// Unmount reverses Mount for a guest_path. The host-side session dies on
// its own when the host's end of the vsock is closed, but we still need
// to unwire the Linux mount namespace.
func (m *Mounter) Unmount(guestPath string) error {
	m.mu.Lock()
	delete(m.active, guestPath)
	m.mu.Unlock()
	out, err := exec.Command("umount", guestPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("umount %s: %s: %w", guestPath, out, err)
	}
	return nil
}

func readFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "<read error: " + err.Error() + ">"
	}
	return string(b)
}

func grepLines(s, substr string) string {
	var matches []string
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, substr) {
			matches = append(matches, line)
		}
	}
	if len(matches) == 0 {
		return "(none)"
	}
	return strings.Join(matches, " | ")
}

func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func writeAname(w interface {
	Write(p []byte) (int, error)
}, aname string) error {
	if len(aname) > 0xFFFF {
		return fmt.Errorf("aname too long")
	}
	var hdr [2]byte
	binary.BigEndian.PutUint16(hdr[:], uint16(len(aname)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write([]byte(aname))
	return err
}
