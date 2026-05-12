//go:build !darwin

package avf

import (
	"fmt"

	vmapi "github.com/anthropics/agent-sandbox/as-hostd/vm"
)

type Backend struct {
	BootDir string
}

func New(bootDir string) *Backend {
	return &Backend{BootDir: bootDir}
}

func (b *Backend) Available() bool { return false }

func (b *Backend) Create(cfg vmapi.Config) (vmapi.VM, error) {
	return nil, fmt.Errorf("avf backend not supported on this platform")
}
