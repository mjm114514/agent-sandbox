//go:build darwin && cgo

// Package avf implements the macOS Apple Virtualization.framework backend.
//
// Boot disks must be raw images (.img) — Virtualization.framework's
// VZDiskImageStorageDeviceAttachment does not understand VHDX. The image
// build pipeline produces:
//
//   <bootDir>/vmlinuz
//   <bootDir>/initramfs
//   <bootDir>/rootfs.img        (rw, attached as /dev/vda)
//   <bootDir>/as-guestpack.img  (ro, attached as /dev/vdb when present)
//
// vsock is exposed via VZVirtioSocketDevice — macOS has no /dev/vsock on the
// host side, so listeners are registered through the framework's
// setSocketListener:forPort: API and connections are surfaced as dup'd
// SOCK_STREAM file descriptors that Go wraps with net.FileConn.
package avf

/*
#cgo CFLAGS: -x objective-c -fobjc-arc -mmacosx-version-min=12.0
#cgo LDFLAGS: -framework Foundation -framework Virtualization
#include <stdlib.h>
#include "bridge_darwin.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/unix"

	vmapi "github.com/anthropics/agent-sandbox/as-hostd/vm"
)

type Backend struct {
	BootDir string
}

func New(bootDir string) *Backend {
	return &Backend{BootDir: bootDir}
}

// Available is true on macOS at compile time. We don't probe the runtime
// macOS version or CPU architecture here; a too-old OS or wrong CPU surfaces
// as a Validate error at Create time, which is clearer than a binary refusal.
func (b *Backend) Available() bool { return true }

func (b *Backend) Create(cfg vmapi.Config) (vmapi.VM, error) {
	kernel := filepath.Join(b.BootDir, "vmlinuz")
	initrd := filepath.Join(b.BootDir, "initramfs")
	baseRootfs := filepath.Join(b.BootDir, "rootfs.img")
	guestpack := filepath.Join(b.BootDir, "as-guestpack.img")

	if _, err := os.Stat(kernel); err != nil {
		return nil, fmt.Errorf("kernel %s: %w", kernel, err)
	}
	if _, err := os.Stat(baseRootfs); err != nil {
		return nil, fmt.Errorf("rootfs %s: %w", baseRootfs, err)
	}

	hasInitrd := true
	if _, err := os.Stat(initrd); err != nil {
		log.Printf("initrd not found at %s — booting kernel without an initial ramdisk (kernel must support root=/dev/vda directly)", initrd)
		hasInitrd = false
	}

	hasGuestpack := false
	if _, err := os.Stat(guestpack); err == nil {
		hasGuestpack = true
	} else {
		log.Printf("as-guestpack not found at %s — falling back to as-guestd baked into rootfs", guestpack)
	}

	id := fmt.Sprintf("sandbox-avf-%d", nextID())

	// Per-VM CoW of the base rootfs so concurrent VMs don't trample each
	// other's writes and the base image stays clean across runs. Mirrors HCS's
	// differencing-VHDX flow. clonefile(2) is an APFS reflink — near-free on
	// APFS, returns ENOTSUP / EXDEV elsewhere (we fall back to a full copy).
	rootfsClone := filepath.Join(os.TempDir(), id+"-rootfs.img")
	if err := cloneRootfs(baseRootfs, rootfsClone); err != nil {
		return nil, fmt.Errorf("clone rootfs %s -> %s: %w", baseRootfs, rootfsClone, err)
	}

	// console=hvc0 because VZVirtioConsoleDeviceSerialPort surfaces as
	// /dev/hvc0 in the guest. root=/dev/vda because the rootfs is the first
	// virtio-blk device.
	cmdline := "console=hvc0 root=/dev/vda rootfstype=ext4 rw init=/sbin/init quiet panic=-1"

	kernelC := C.CString(kernel)
	initrdC := C.CString("")
	if hasInitrd {
		C.free(unsafe.Pointer(initrdC))
		initrdC = C.CString(initrd)
	}
	cmdlineC := C.CString(cmdline)
	rootfsC := C.CString(rootfsClone)
	guestpackC := C.CString("")
	if hasGuestpack {
		C.free(unsafe.Pointer(guestpackC))
		guestpackC = C.CString(guestpack)
	}
	defer C.free(unsafe.Pointer(kernelC))
	defer C.free(unsafe.Pointer(initrdC))
	defer C.free(unsafe.Pointer(cmdlineC))
	defer C.free(unsafe.Pointer(rootfsC))
	defer C.free(unsafe.Pointer(guestpackC))

	memBytes := uint64(cfg.MemoryMB) * 1024 * 1024
	if memBytes == 0 {
		memBytes = 8 * 1024 * 1024 * 1024
	}

	ccfg := C.avf_vm_config_t{
		kernel_path:    kernelC,
		initrd_path:    initrdC,
		cmdline:        cmdlineC,
		rootfs_path:    rootfsC,
		guestpack_path: guestpackC,
		vcpus:          C.int(cfg.VCPUs),
		memory_bytes:   C.uint64_t(memBytes),
	}

	var cerr *C.char
	handle := C.avf_vm_create(&ccfg, &cerr)
	if handle == nil {
		msg := C.GoString(cerr)
		C.avf_free_str(cerr)
		os.Remove(rootfsClone)
		return nil, fmt.Errorf("avf_vm_create: %s", msg)
	}

	return &avfVM{
		id:          id,
		handle:      handle,
		cfg:         cfg,
		state:       vmapi.StateStopped,
		rootfsClone: rootfsClone,
		listeners:   make(map[uint32]*avfListener),
	}, nil
}

// cloneRootfs uses clonefile(2) for an APFS reflink, falling back to a regular
// byte copy when the source and target live on a filesystem that can't share
// blocks (ENOTSUP / EXDEV).
func cloneRootfs(src, dst string) error {
	_ = os.Remove(dst) // clonefile requires dst to not exist
	err := unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW)
	if err == nil {
		return nil
	}
	if err != unix.ENOTSUP && err != unix.EXDEV {
		return err
	}
	// Cross-filesystem or non-APFS — fall back to a full copy.
	srcF, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcF.Close()
	dstF, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer dstF.Close()
	buf := make([]byte, 1<<20)
	for {
		n, rerr := srcF.Read(buf)
		if n > 0 {
			if _, werr := dstF.Write(buf[:n]); werr != nil {
				return werr
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return nil
			}
			return rerr
		}
	}
}

type avfVM struct {
	mu          sync.Mutex
	id          string
	handle      C.avf_vm_t
	cfg         vmapi.Config
	state       vmapi.State
	rootfsClone string // per-VM CoW of the base rootfs; removed on Destroy
	listeners   map[uint32]*avfListener
}

func (v *avfVM) ID() string { return v.id }

func (v *avfVM) Start() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.handle == nil {
		return fmt.Errorf("VM destroyed")
	}
	v.state = vmapi.StateStarting

	var cerr *C.char
	if rc := C.avf_vm_start(v.handle, &cerr); rc != 0 {
		msg := C.GoString(cerr)
		C.avf_free_str(cerr)
		v.state = vmapi.StateStopped
		return fmt.Errorf("avf_vm_start: %s", msg)
	}
	v.state = vmapi.StateRunning
	return nil
}

func (v *avfVM) Stop() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.state != vmapi.StateRunning && v.state != vmapi.StateStarting {
		return nil
	}
	if v.handle == nil {
		v.state = vmapi.StateStopped
		return nil
	}
	v.state = vmapi.StateStopping

	var cerr *C.char
	if rc := C.avf_vm_stop(v.handle, &cerr); rc != 0 {
		msg := C.GoString(cerr)
		C.avf_free_str(cerr)
		// Keep state as Stopping so the caller can retry or destroy.
		return fmt.Errorf("avf_vm_stop: %s", msg)
	}
	v.state = vmapi.StateStopped
	return nil
}

func (v *avfVM) Destroy() error {
	_ = v.Stop()
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, l := range v.listeners {
		l.closeOnce()
	}
	v.listeners = nil
	if v.handle != nil {
		C.avf_vm_destroy(v.handle)
		v.handle = nil
	}
	if v.rootfsClone != "" {
		if err := os.Remove(v.rootfsClone); err != nil && !os.IsNotExist(err) {
			log.Printf("remove rootfs clone %s: %v", v.rootfsClone, err)
		}
		v.rootfsClone = ""
	}
	return nil
}

func (v *avfVM) VSockListen(port uint32) (net.Listener, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.handle == nil {
		return nil, fmt.Errorf("VM destroyed")
	}
	// Re-listen on a port whose previous listener is still open: hand back
	// the live listener. If it was closed, fall through and rebuild — we
	// remove the stale framework registration first so setSocketListener
	// doesn't silently overwrite an old delegate.
	if l, ok := v.listeners[port]; ok {
		if !l.closed.Load() {
			return l, nil
		}
		C.avf_vm_unlisten(v.handle, C.uint32_t(port))
		delete(v.listeners, port)
	}
	var cerr *C.char
	lh := C.avf_vm_listen(v.handle, C.uint32_t(port), &cerr)
	if lh == nil {
		msg := C.GoString(cerr)
		C.avf_free_str(cerr)
		return nil, fmt.Errorf("avf_vm_listen port %d: %s", port, msg)
	}
	l := &avfListener{port: port, handle: lh}
	v.listeners[port] = l
	return l, nil
}

// ShareDir on AVF is a no-op: sandbox-level mounts are routed through the
// 9P-over-vsock fileshare server (port 1002) in the as-hostd daemon, the same
// path used on Windows. virtio-fs would be the AVF-native alternative but is
// intentionally not used so the mount/file-guard plumbing stays single-path.
func (v *avfVM) ShareDir(tag, hostPath string) error {
	return nil
}

func (v *avfVM) State() vmapi.State {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.state
}

type avfListener struct {
	port   uint32
	handle C.avf_listener_t
	closed atomic.Bool
}

func (l *avfListener) Accept() (net.Conn, error) {
	if l.closed.Load() {
		return nil, net.ErrClosed
	}
	fd := int(C.avf_listener_accept(l.handle))
	if fd < 0 {
		return nil, net.ErrClosed
	}
	// FileConn dup's the fd; close our copy after to avoid leaking.
	f := os.NewFile(uintptr(fd), fmt.Sprintf("vsock-%d", l.port))
	if f == nil {
		return nil, fmt.Errorf("os.NewFile returned nil for fd %d", fd)
	}
	conn, err := net.FileConn(f)
	f.Close()
	if err != nil {
		return nil, fmt.Errorf("wrap vsock fd: %w", err)
	}
	return conn, nil
}

func (l *avfListener) Close() error {
	l.closeOnce()
	return nil
}

func (l *avfListener) closeOnce() {
	if l.closed.Swap(true) {
		return
	}
	C.avf_listener_close(l.handle)
}

func (l *avfListener) Addr() net.Addr { return &vsockAddr{port: l.port} }

type vsockAddr struct{ port uint32 }

func (a *vsockAddr) Network() string { return "vsock" }
func (a *vsockAddr) String() string  { return fmt.Sprintf("vsock://*:%d", a.port) }

var (
	idCounter uint64
	idMu      sync.Mutex
)

func nextID() uint64 {
	idMu.Lock()
	defer idMu.Unlock()
	idCounter++
	return idCounter
}
