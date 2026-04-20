package hcs

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	winio "github.com/Microsoft/go-winio"
	"github.com/Microsoft/go-winio/pkg/guid"

	vmapi "github.com/anthropics/agent-sandbox/sandboxd/vm"
)

var (
	computecore                           = syscall.NewLazyDLL("computecore.dll")
	procHcsCreateOperation                = computecore.NewProc("HcsCreateOperation")
	procHcsCreateComputeSystem            = computecore.NewProc("HcsCreateComputeSystem")
	procHcsStartComputeSystem             = computecore.NewProc("HcsStartComputeSystem")
	procHcsTerminateComputeSystem         = computecore.NewProc("HcsTerminateComputeSystem")
	procHcsCloseComputeSystem             = computecore.NewProc("HcsCloseComputeSystem")
	procHcsCloseOperation                 = computecore.NewProc("HcsCloseOperation")
	procHcsWaitForOperationResult         = computecore.NewProc("HcsWaitForOperationResult")
	procHcsGetComputeSystemProperties     = computecore.NewProc("HcsGetComputeSystemProperties")
)

var _ vmapi.VM = (*hcsVM)(nil)

type Backend struct {
	BootDir string
}

func New(bootDir string) *Backend {
	return &Backend{BootDir: bootDir}
}

func (b *Backend) Available() bool {
	return computecore.Load() == nil
}

func (b *Backend) Create(cfg vmapi.Config) (vmapi.VM, error) {
	id := fmt.Sprintf("sandbox-%d", nextID())

	kernelPath := filepath.Join(b.BootDir, "vmlinuz")
	initrdPath := filepath.Join(b.BootDir, "initramfs")
	rootfsPath := filepath.Join(b.BootDir, "rootfs.vhdx")

	absKernel, _ := filepath.Abs(kernelPath)
	absInitrd, _ := filepath.Abs(initrdPath)
	absRootfs, _ := filepath.Abs(rootfsPath)

	// Build vsock port list
	vsockPorts := make(map[uint32]bool)
	vsockPorts[1000] = true // control
	vsockPorts[1001] = true // data
	for _, p := range cfg.VSockPorts {
		vsockPorts[p] = true
	}

	// HCS document for a LCOW utility VM
	doc := map[string]any{
		"Owner":         "agent-sandbox",
		"SchemaVersion": map[string]any{"Major": 2, "Minor": 1},
		"ShouldTerminateOnLastHandleClosed": true,
		"VirtualMachine": map[string]any{
			"StopOnReset": true,
			"Chipset": map[string]any{
				"LinuxKernelDirect": map[string]any{
					"KernelFilePath": absKernel,
					"InitRdPath":     absInitrd,
					"KernelCmdLine":  "console=hvc0 root=/dev/sda rootfstype=ext4 rootdelay=5 rw init=/sbin/init quiet panic=-1 modules=hv_storvsc,hv_vmbus,hv_sock,vsock",
				},
			},
			"ComputeTopology": map[string]any{
				"Memory": map[string]any{
					"SizeInMB":        cfg.MemoryMB,
					"AllowOvercommit": true,
				},
				"Processor": map[string]any{
					"Count": cfg.VCPUs,
				},
			},
			"Devices": map[string]any{
				"Scsi": map[string]any{
					"0": map[string]any{
						"Attachments": map[string]any{
							"0": map[string]any{
								"Type": "VirtualDisk",
								"Path":  absRootfs,
							},
						},
					},
				},
				"HvSocket": map[string]any{
					"HvSocketConfig": map[string]any{
						"DefaultBindSecurityDescriptor": "D:P(A;;FA;;;WD)",
					},
				},
				"Plan9": map[string]any{},
			},
		},
	}

	docBytes, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("marshal HCS document: %w", err)
	}

	vm := &hcsVM{
		id:      id,
		cfg:     cfg,
		state:   vmapi.StateStopped,
		shares:  make(map[string]string),
		bootDir: b.BootDir,
	}

	docUTF16, err := syscall.UTF16PtrFromString(string(docBytes))
	if err != nil {
		return nil, err
	}
	idUTF16, err := syscall.UTF16PtrFromString(id)
	if err != nil {
		return nil, err
	}

	op := createOperation()
	defer closeOperation(op)

	var handle syscall.Handle
	var resultDoc *uint16

	r1, _, _ := procHcsCreateComputeSystem.Call(
		uintptr(unsafe.Pointer(idUTF16)),
		uintptr(unsafe.Pointer(docUTF16)),
		op,
		0, // security descriptor
		uintptr(unsafe.Pointer(&handle)),
		uintptr(unsafe.Pointer(&resultDoc)),
	)
	if r1 != 0 {
		return nil, fmt.Errorf("HcsCreateComputeSystem: HRESULT 0x%08x, result: %s", r1, readUTF16Ptr(resultDoc))
	}

	if err := waitForOperation(op, 60000); err != nil {
		return nil, fmt.Errorf("HcsCreateComputeSystem wait: %w", err)
	}

	vm.handle = handle
	return vm, nil
}

type hcsVM struct {
	mu      sync.Mutex
	id      string
	handle  syscall.Handle
	cfg     vmapi.Config
	state   vmapi.State
	shares  map[string]string
	bootDir string
	vmID    guid.GUID
}

func (v *hcsVM) ID() string {
	return v.id
}

func (v *hcsVM) Start() error {
	v.mu.Lock()
	defer v.mu.Unlock()

	v.state = vmapi.StateStarting

	op := createOperation()
	defer closeOperation(op)

	var resultDoc *uint16
	r1, _, _ := procHcsStartComputeSystem.Call(
		uintptr(v.handle),
		op,
		uintptr(unsafe.Pointer(&resultDoc)),
	)
	if r1 != 0 {
		v.state = vmapi.StateStopped
		return fmt.Errorf("HcsStartComputeSystem: HRESULT 0x%08x, result: %s", r1, readUTF16Ptr(resultDoc))
	}

	if err := waitForOperation(op, 60000); err != nil {
		v.state = vmapi.StateStopped
		return fmt.Errorf("HcsStartComputeSystem wait: %w", err)
	}

	v.vmID = v.getRuntimeID()
	v.state = vmapi.StateRunning
	return nil
}

func (v *hcsVM) getRuntimeID() guid.GUID {
	out, err := exec.Command("hcsdiag.exe", "list").Output()
	if err != nil {
		log.Printf("hcsdiag list failed: %v", err)
		return guid.GUID{}
	}
	lines := strings.Split(string(out), "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) == v.id && i+1 < len(lines) {
			nextLine := lines[i+1]
			parts := strings.Split(nextLine, ",")
			for _, p := range parts {
				p = strings.TrimSpace(p)
				if len(p) == 36 && p[8] == '-' {
					if g, err := guid.FromString(p); err == nil {
						return g
					}
				}
			}
		}
	}
	log.Printf("VM GUID not found for %s", v.id)
	return guid.GUID{}
}

func (v *hcsVM) Stop() error {
	v.mu.Lock()
	defer v.mu.Unlock()

	if v.state != vmapi.StateRunning && v.state != vmapi.StateStarting {
		return nil
	}

	v.state = vmapi.StateStopping

	op := createOperation()
	defer closeOperation(op)

	var resultDoc *uint16
	r1, _, _ := procHcsTerminateComputeSystem.Call(
		uintptr(v.handle),
		op,
		uintptr(unsafe.Pointer(&resultDoc)),
	)
	if r1 != 0 && r1 != 0xc0370108 { // ignore HCS_E_CONNECTION_CLOSED
		return fmt.Errorf("HcsTerminateComputeSystem: HRESULT 0x%08x", r1)
	}
	waitForOperation(op, 30000)
	v.state = vmapi.StateStopped
	return nil
}

func (v *hcsVM) Destroy() error {
	v.Stop()
	procHcsCloseComputeSystem.Call(uintptr(v.handle))
	return nil
}

func (v *hcsVM) VSockListen(port uint32) (net.Listener, error) {
	serviceID := vsockPortToServiceGUID(port)
	return winio.ListenHvsock(&winio.HvsockAddr{
		VMID:      v.vmID,
		ServiceID: serviceID,
	})
}

func (v *hcsVM) ShareDir(tag string, hostPath string) error {
	// Plan 9 shares are added via HCS modify request.
	// For now, a simple implementation using the Scsi path.
	v.mu.Lock()
	defer v.mu.Unlock()
	v.shares[tag] = hostPath
	return nil
}

func (v *hcsVM) State() vmapi.State {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.state
}

func vsockPortToServiceGUID(port uint32) guid.GUID {
	return guid.GUID{
		Data1: port,
		Data2: 0xFACB,
		Data3: 0x11E6,
		Data4: [8]byte{0xBD, 0x58, 0x64, 0x00, 0x6A, 0x79, 0x86, 0xD3},
	}
}

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

func createOperation() uintptr {
	op, _, _ := procHcsCreateOperation.Call(0, 0)
	return op
}

func closeOperation(op uintptr) {
	procHcsCloseOperation.Call(op)
}

func waitForOperation(op uintptr, timeoutMs uint32) error {
	var resultDoc *uint16
	r1, _, _ := procHcsWaitForOperationResult.Call(
		op,
		uintptr(timeoutMs),
		uintptr(unsafe.Pointer(&resultDoc)),
	)
	if r1 != 0 {
		return fmt.Errorf("HRESULT 0x%08x: %s", r1, readUTF16Ptr(resultDoc))
	}
	return nil
}

func readUTF16Ptr(p *uint16) string {
	if p == nil {
		return ""
	}
	return syscall.UTF16ToString((*[1 << 16]uint16)(unsafe.Pointer(p))[:])
}
