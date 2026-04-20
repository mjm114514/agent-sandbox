package vm

import "net"

type Config struct {
	Backend    string
	VCPUs      int
	MemoryMB   int
	KernelPath string
	RootFSPath string
	Mounts     []SharedDir
	VSockPorts []uint32
}

type SharedDir struct {
	Tag      string
	HostPath string
}

type VM interface {
	ID() string
	Start() error
	Stop() error
	Destroy() error
	VSockListen(port uint32) (net.Listener, error)
	ShareDir(tag string, hostPath string) error
	State() State
}

type State int

const (
	StateStopped State = iota
	StateStarting
	StateRunning
	StateStopping
)

type Backend interface {
	Create(cfg Config) (VM, error)
	Available() bool
}
